package cli

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/tao-vin/opsera/internal/config"
	"github.com/tao-vin/opsera/internal/crypto"
	"github.com/tao-vin/opsera/internal/events"
	"github.com/tao-vin/opsera/internal/logs"
	"github.com/tao-vin/opsera/internal/model"
	"github.com/tao-vin/opsera/internal/xsh"
)

var xshPathPattern = regexp.MustCompile(`[A-Za-z]:\\[^"]+\.xsh`)

const ssoAgentBaseURL = "http://127.0.0.1:18742"

func TryRun(args []string) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	if isCLIHelpArg(args[0]) {
		fmt.Fprintln(os.Stdout, cliUsage())
		return true, nil
	}
	if isCLIVersionArg(args[0]) {
		fmt.Fprintln(os.Stdout, "opsera")
		return true, nil
	}
	if args[0] == "sso" {
		return runSSO(args[1:])
	}
	if args[0] == "file" {
		return runFile(args[1:])
	}
	if args[0] != "command" {
		return false, nil
	}
	if len(args) < 3 || args[1] != "run" {
		return true, errors.New("usage: opsera command run [--server <name|host|id>] [--xsh <path>] [--sso] <shell-command>")
	}
	serverRef := ""
	xshPath := ""
	useSSO := false
	commandArgs := args[2:]
	for len(commandArgs) >= 2 {
		if commandArgs[0] == "--server" {
			serverRef = commandArgs[1]
			commandArgs = commandArgs[2:]
			continue
		}
		if commandArgs[0] == "--xsh" {
			xshPath = commandArgs[1]
			commandArgs = commandArgs[2:]
			continue
		}
		break
	}
	for len(commandArgs) >= 1 {
		if commandArgs[0] == "--sso" || commandArgs[0] == "--dasusm" {
			useSSO = true
			commandArgs = commandArgs[1:]
			continue
		}
		break
	}
	command := strings.TrimSpace(strings.Join(commandArgs, " "))
	if command == "" {
		return true, errors.New("command is required")
	}
	dataDir, err := resolveDataDir()
	if err != nil {
		return true, err
	}
	logStore, err := logs.NewStore(filepath.Join(dataDir, "logs"))
	if err != nil {
		return true, err
	}
	item := model.Command{
		ID:        fmt.Sprintf("cmd-%d", time.Now().UnixNano()),
		Command:   command,
		CreatedAt: time.Now().Format(time.RFC3339),
		UpdatedAt: time.Now().Format(time.RFC3339),
	}
	var output string
	var runErr error
	if strings.TrimSpace(serverRef) != "" {
		if apiItem, err := runServerCommandViaAPI(serverRef, command); err == nil {
			_ = logStore.Append(model.LogLevelInfo, "command", "done via opsera api: "+command, apiItem.ID)
			return true, writeJSON(apiItem)
		}
		output, runErr = RunServerCommand(serverRef, command)
	} else if useSSO {
		output, runErr = RunDASUSMCommand(command)
	} else {
		output, runErr = RunCommand(xshPath, command)
	}
	item.Output = output
	if runErr != nil {
		item.Status = model.CommandStatusFailed
		item.Error = runErr.Error()
		_ = logStore.Append(model.LogLevelError, "command", "failed: "+command+": "+runErr.Error(), item.ID)
		_ = events.Write(dataDir, events.Event{
			Type:    "command",
			Command: command,
			Output:  output,
			Error:   runErr.Error(),
			Status:  string(item.Status),
		})
		_ = json.NewEncoder(os.Stdout).Encode(item)
		return true, runErr
	}
	item.Status = model.CommandStatusDone
	_ = logStore.Append(model.LogLevelInfo, "command", "done: "+command, item.ID)
	_ = events.Write(dataDir, events.Event{
		Type:    "command",
		Command: command,
		Output:  output,
		Status:  string(item.Status),
	})
	return true, writeJSON(item)
}

func writeJSON(value any) error {
	err := json.NewEncoder(os.Stdout).Encode(value)
	if isInvalidStdout(err) {
		return nil
	}
	return err
}

func isInvalidStdout(err error) bool {
	return err != nil && errors.Is(err, syscall.Errno(6))
}

func isCLIHelpArg(arg string) bool {
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "help", "--help", "-h", "/h", "/?":
		return true
	default:
		return false
	}
}

func isCLIVersionArg(arg string) bool {
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "version", "--version", "-version", "/version":
		return true
	default:
		return false
	}
}

func cliUsage() string {
	return strings.Join([]string{
		"usage:",
		"  opsera command run [--server <name|host|id>] [--xsh <path>] [--sso] <shell-command>",
		"  opsera sso attach [--core-only] [--window-keepalive]",
		"  opsera sso agent [--core-only] [--window-keepalive]",
		"  opsera file upload [--server <name|host|id>] [--xsh <path>] [--sso] <local> <remote>",
		"  opsera file upload-large [--xsh <path>] [--sso] [--chunk-mb 512] <local> <remote>",
		"  opsera file download [--xsh <path>] [--sso] <remote> <local>",
	}, "\n")
}

func runSSO(args []string) (bool, error) {
	if len(args) == 0 {
		return true, errors.New("usage: opsera sso attach|agent [--core-only]")
	}
	switch args[0] {
	case "agent":
		opts, err := parseSSOOptions(args[1:])
		if err != nil {
			return true, err
		}
		return true, runSSOAgent(opts)
	case "attach":
		opts, err := parseSSOOptions(args[1:])
		if err != nil {
			return true, err
		}
		if opts.CoreOnly {
			_ = os.Setenv("OPSERA_SSO_CORE_ONLY", "1")
		}
		if opts.WindowKeepAlive {
			_ = os.Setenv("OPSERA_XSHELL_WINDOW_KEEPALIVE", "1")
		}
		result, handled, err := callSSOAgent("POST", "/sso/attach", nil, 10*time.Second)
		if !handled {
			return true, err
		}
		if err != nil {
			_ = writeJSON(result)
			return true, err
		}
		return true, writeJSON(result)
	default:
		return true, errors.New("usage: opsera sso attach|agent [--core-only]")
	}
}

type ssoOptions struct {
	CoreOnly        bool
	WindowKeepAlive bool
}

func parseSSOOptions(args []string) (ssoOptions, error) {
	opts := ssoOptions{}
	for _, arg := range args {
		switch arg {
		case "--core-only":
			opts.CoreOnly = true
		case "--window-keepalive":
			opts.WindowKeepAlive = true
		default:
			return opts, fmt.Errorf("unknown sso option: %s", arg)
		}
	}
	return opts, nil
}

func runSSOAgent(opts ssoOptions) error {
	dataDir, err := resolveDataDir()
	if err != nil {
		return err
	}
	logStore, err := logs.NewStore(filepath.Join(dataDir, "logs"))
	if err != nil {
		return err
	}
	pool := NewDASUSMPool(logStore)
	pool.coreOnly = opts.CoreOnly || envBool("OPSERA_SSO_CORE_ONLY")
	go autoAttachDASUSM(pool, logStore)
	if opts.WindowKeepAlive || envBool("OPSERA_XSHELL_WINDOW_KEEPALIVE") {
		go keepXshellWindowActive(logStore, time.Minute, "ls")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":        true,
			"connected": pool.Connected(),
		})
	})
	mux.HandleFunc("/sso/attach", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		err := pool.Attach()
		writeSSOAgentResult(w, map[string]any{"status": statusFromErr(err), "connected": err == nil}, err)
	})
	mux.HandleFunc("/sso/command/run", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Command string `json:"command"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		req.Command = strings.TrimSpace(req.Command)
		if req.Command == "" {
			http.Error(w, "command is required", http.StatusBadRequest)
			return
		}
		item := model.Command{
			ID:        fmt.Sprintf("cmd-%d", time.Now().UnixNano()),
			Command:   req.Command,
			CreatedAt: time.Now().Format(time.RFC3339),
			UpdatedAt: time.Now().Format(time.RFC3339),
		}
		output, err := pool.Run(req.Command)
		item.Output = output
		if err != nil {
			item.Status = model.CommandStatusFailed
			item.Error = err.Error()
			_ = json.NewEncoder(w).Encode(item)
			return
		}
		item.Status = model.CommandStatusDone
		_ = json.NewEncoder(w).Encode(item)
	})
	mux.HandleFunc("/sso/file/upload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Local  string `json:"local"`
			Remote string `json:"remote"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		err := pool.Upload(req.Local, req.Remote)
		writeSSOAgentResult(w, map[string]any{"status": statusFromErr(err), "local": req.Local, "remote": req.Remote}, err)
	})
	mux.HandleFunc("/sso/file/download", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Remote string `json:"remote"`
			Local  string `json:"local"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		err := pool.Download(req.Remote, req.Local)
		writeSSOAgentResult(w, map[string]any{"status": statusFromErr(err), "remote": req.Remote, "local": req.Local}, err)
	})
	mux.HandleFunc("/sso/file/upload-large", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Local      string `json:"local"`
			Remote     string `json:"remote"`
			ChunkBytes int64  `json:"chunkBytes"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.ChunkBytes <= 0 {
			req.ChunkBytes = 512 * 1024 * 1024
		}
		err := pool.UploadLarge(req.Local, req.Remote, req.ChunkBytes)
		writeSSOAgentResult(w, map[string]any{"status": statusFromErr(err), "local": req.Local, "remote": req.Remote, "chunkBytes": req.ChunkBytes}, err)
	})
	server := &http.Server{
		Addr:    "127.0.0.1:18742",
		Handler: mux,
	}
	_ = logStore.Append(model.LogLevelInfo, "sso", "agent listening on 127.0.0.1:18742", "")
	return server.ListenAndServe()
}

func writeSSOAgentResult(w http.ResponseWriter, result map[string]any, err error) {
	if err != nil {
		result["error"] = err.Error()
	}
	_ = json.NewEncoder(w).Encode(result)
}

func statusFromErr(err error) string {
	if err != nil {
		return "failed"
	}
	return "done"
}

func callSSOAgent(method, path string, body any, timeout time.Duration) (map[string]any, bool, error) {
	if err := ensureSSOAgent(); err != nil {
		return nil, false, err
	}
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, true, err
		}
		reader = bytes.NewReader(data)
	}
	client := http.Client{Timeout: timeout}
	req, err := http.NewRequest(method, ssoAgentBaseURL+path, reader)
	if err != nil {
		return nil, true, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, true, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if msg, _ := result["error"].(string); msg != "" {
			return result, true, errors.New(msg)
		}
		return result, true, fmt.Errorf("sso agent returned %s", resp.Status)
	}
	if status, _ := result["status"].(string); status == "failed" {
		if msg, _ := result["error"].(string); msg != "" {
			return result, true, errors.New(msg)
		}
		return result, true, errors.New("sso agent operation failed")
	}
	return result, true, nil
}

func callSSOAgentCommand(command string) (model.Command, bool, error) {
	if err := ensureSSOAgent(); err != nil {
		return model.Command{}, false, err
	}
	body, err := json.Marshal(map[string]string{"command": command})
	if err != nil {
		return model.Command{}, true, err
	}
	client := http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Post(ssoAgentBaseURL+"/sso/command/run", "application/json", bytes.NewReader(body))
	if err != nil {
		return model.Command{}, false, err
	}
	defer resp.Body.Close()
	var item model.Command
	if err := json.NewDecoder(resp.Body).Decode(&item); err != nil {
		return item, true, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return item, true, fmt.Errorf("sso agent returned %s", resp.Status)
	}
	if item.Status == model.CommandStatusFailed {
		return item, true, errors.New(item.Error)
	}
	return item, true, nil
}

func ensureSSOAgent() error {
	if ssoAgentHealthy() {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "sso", "agent")
	env := os.Environ()
	if envBool("OPSERA_SSO_CORE_ONLY") {
		env = append(env, "OPSERA_SSO_CORE_ONLY=1")
	}
	if envBool("OPSERA_XSHELL_WINDOW_KEEPALIVE") {
		env = append(env, "OPSERA_XSHELL_WINDOW_KEEPALIVE=1")
	}
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ssoAgentHealthy() {
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return errors.New("sso agent did not become ready")
}

func ssoAgentHealthy() bool {
	client := http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get(ssoAgentBaseURL + "/health")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func autoAttachDASUSM(pool *DASUSMPool, logStore *logs.Store) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	lastSignature := ""
	lastAttempt := time.Time{}
	lastErrorLog := time.Time{}
	for range ticker.C {
		if pool.Connected() {
			continue
		}
		signature := xshellProcessSignature()
		if signature == "" {
			continue
		}
		if signature == lastSignature && time.Since(lastAttempt) < 10*time.Second {
			continue
		}
		lastSignature = signature
		lastAttempt = time.Now()
		if err := pool.Attach(); err != nil {
			if logStore != nil && time.Since(lastErrorLog) > time.Minute {
				_ = logStore.Append(model.LogLevelWarn, "sso", "auto attach failed: "+err.Error(), "")
				lastErrorLog = time.Now()
			}
			continue
		}
		if logStore != nil {
			_ = logStore.Append(model.LogLevelInfo, "sso", "auto attach ready", "")
		}
	}
}

func keepXshellWindowActive(logStore *logs.Store, interval time.Duration, command string) {
	if interval <= 0 {
		interval = time.Minute
	}
	command = strings.TrimSpace(command)
	if command == "" {
		command = "ls"
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	lastErrorLog := time.Time{}
	for {
		if err := sendCommandToXshellWindow(command); err != nil {
			if logStore != nil && time.Since(lastErrorLog) > time.Minute {
				_ = logStore.Append(model.LogLevelWarn, "sso", "xshell window keepalive failed: "+err.Error(), "")
				lastErrorLog = time.Now()
			}
		}
		<-ticker.C
	}
}

func sendCommandToXshellWindow(command string) error {
	escapedCommand := strings.ReplaceAll(command, "'", "''")
	script := fmt.Sprintf(`$ErrorActionPreference = 'Stop'
Add-Type @"
using System;
using System.Runtime.InteropServices;
public class OpseraWin32 {
  [DllImport("user32.dll")] public static extern IntPtr GetForegroundWindow();
  [DllImport("user32.dll")] public static extern bool SetForegroundWindow(IntPtr hWnd);
  [DllImport("user32.dll")] public static extern bool IsWindow(IntPtr hWnd);
}
"@
$ws = New-Object -ComObject WScript.Shell
$previous = [OpseraWin32]::GetForegroundWindow()
$process = Get-Process XshellCore,Xshell -ErrorAction SilentlyContinue |
  Where-Object { $_.MainWindowHandle -ne 0 } |
  Sort-Object @{Expression={ if ($_.ProcessName -eq 'XshellCore') { 0 } else { 1 } }}, Id -Descending |
  Select-Object -First 1
if ($null -eq $process) { throw 'no visible Xshell window found' }
if (-not $ws.AppActivate($process.Id)) { throw 'could not activate Xshell window' }
Start-Sleep -Milliseconds 200
$ws.SendKeys('%s{ENTER}')
Start-Sleep -Milliseconds 100
if ($previous -ne [IntPtr]::Zero -and $previous -ne $process.MainWindowHandle -and [OpseraWin32]::IsWindow($previous)) {
  [void][OpseraWin32]::SetForegroundWindow($previous)
}
`, escapedCommand)
	out, err := exec.Command("powershell.exe", "-NoProfile", "-STA", "-Command", script).CombinedOutput()
	if err != nil {
		text := strings.TrimSpace(string(out))
		if text == "" {
			text = err.Error()
		}
		return errors.New(text)
	}
	return nil
}

func xshellProcessSignature() string {
	processes, err := latestXshellProcessSnapshots()
	if err != nil || len(processes) == 0 {
		return ""
	}
	hash := sha256.New()
	wrote := false
	for _, process := range processes {
		if !strings.Contains(process.CommandLine, "ssh://") {
			continue
		}
		_, _ = fmt.Fprintf(hash, "%d\x00%s\x00%s\x00", process.ProcessID, process.Name, process.CommandLine)
		wrote = true
	}
	if !wrote {
		return ""
	}
	sum := hash.Sum(nil)
	return hex.EncodeToString(sum)
}

func RunCommand(xshPath string, command string) (string, error) {
	session, err := LatestSession(xshPath)
	if err != nil {
		return "", err
	}
	client, err := dialSession(session)
	if err != nil {
		return "", err
	}
	defer client.Close()
	sshSession, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer sshSession.Close()
	out, err := sshSession.CombinedOutput(command)
	return string(out), err
}

func RunServerCommand(serverRef string, command string) (string, error) {
	session, err := serverSession(serverRef)
	if err != nil {
		return "", err
	}
	client, err := dialSession(session)
	if err != nil {
		return "", err
	}
	defer client.Close()
	return remoteRun(client, command)
}

func RunDASUSMCommand(command string) (string, error) {
	if item, handled, err := callSSOAgentCommand(command); handled {
		return item.Output, err
	}
	sessions, err := LatestDASUSMSessions()
	if err != nil {
		return "", err
	}
	failures := []string{}
	for _, session := range sessions {
		client, err := dialSession(session)
		if err != nil {
			failures = append(failures, sessionFailure(session, err))
			continue
		}
		output, runErr := remoteRun(client, command)
		_ = client.Close()
		if runErr != nil {
			failures = append(failures, sessionFailure(session, runErr))
			continue
		}
		return output, nil
	}
	if len(failures) == 0 {
		return "", errors.New("no live Xshell SSH URL found")
	}
	return "", errors.New("all live Xshell SSH URLs failed: " + strings.Join(failures, "; "))
}

type DASUSMPool struct {
	logs *logs.Store

	mu          sync.Mutex
	client      *ssh.Client
	session     xsh.Session
	connectedAt time.Time
	failed      map[string]time.Time
	coreOnly    bool
}

func NewDASUSMPool(logStore *logs.Store) *DASUSMPool {
	return &DASUSMPool{logs: logStore, failed: map[string]time.Time{}}
}

func (p *DASUSMPool) Connected() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.client != nil && sshAlive(p.client)
}

func (p *DASUSMPool) Attach() error {
	client, err := p.clientForUse()
	if err != nil {
		return err
	}
	return checkClient(client)
}

func (p *DASUSMPool) Run(command string) (string, error) {
	client, err := p.clientForUse()
	if err != nil {
		return "", err
	}
	output, err := runWithClient(client, command)
	if err == nil {
		return output, nil
	}
	p.drop()
	client, reconnectErr := p.clientForUse()
	if reconnectErr != nil {
		return output, fmt.Errorf("%w; reconnect failed: %v", err, reconnectErr)
	}
	return runWithClient(client, command)
}

func (p *DASUSMPool) Upload(localPath, remotePath string) error {
	return p.withRetry(func(client *ssh.Client) error {
		return uploadWithClient(client, localPath, remotePath)
	})
}

func (p *DASUSMPool) Download(remotePath, localPath string) error {
	return p.withRetry(func(client *ssh.Client) error {
		return downloadWithClient(client, remotePath, localPath)
	})
}

func (p *DASUSMPool) UploadLarge(localPath, remotePath string, chunkSize int64) error {
	return p.withRetry(func(client *ssh.Client) error {
		return uploadLargeWithClient(client, localPath, remotePath, chunkSize)
	})
}

func (p *DASUSMPool) Close() {
	p.drop()
}

func (p *DASUSMPool) withRetry(run func(*ssh.Client) error) error {
	client, err := p.clientForUse()
	if err != nil {
		return err
	}
	if err := run(client); err == nil {
		return nil
	} else {
		p.drop()
		client, reconnectErr := p.clientForUse()
		if reconnectErr != nil {
			return fmt.Errorf("%w; reconnect failed: %v", err, reconnectErr)
		}
		return run(client)
	}
}

func (p *DASUSMPool) clientForUse() (*ssh.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client != nil && sshAlive(p.client) {
		return p.client, nil
	}
	if p.client != nil {
		_ = p.client.Close()
		p.client = nil
	}
	sessions, err := latestDASUSMSessions(p.coreOnly)
	if err != nil {
		return nil, err
	}
	failures := []string{}
	for _, session := range sessions {
		if p.sessionBlocked(session) {
			continue
		}
		client, err := dialSession(session)
		if err != nil {
			p.markSessionFailed(session)
			failures = append(failures, sessionFailure(session, err))
			continue
		}
		p.client = client
		p.session = session
		p.connectedAt = time.Now()
		if p.logs != nil {
			_ = p.logs.Append(model.LogLevelInfo, "sso", "connection ready: "+session.UserName+"@"+session.Host, "")
		}
		return client, nil
	}
	if len(failures) == 0 {
		return nil, errors.New("all failed Xshell SSH URLs are cooling down; reopen Xshell from SSO or wait for a new XshellCore URL")
	}
	return nil, errors.New("all live Xshell SSH URLs failed: " + strings.Join(failures, "; "))
}

func (p *DASUSMPool) sessionBlocked(session xsh.Session) bool {
	if len(p.failed) == 0 {
		return false
	}
	key := sessionCacheKey(session)
	failedAt, ok := p.failed[key]
	if !ok {
		return false
	}
	if time.Since(failedAt) > 10*time.Minute {
		delete(p.failed, key)
		return false
	}
	return true
}

func (p *DASUSMPool) markSessionFailed(session xsh.Session) {
	if p.failed == nil {
		p.failed = map[string]time.Time{}
	}
	p.failed[sessionCacheKey(session)] = time.Now()
}

func (p *DASUSMPool) drop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client != nil {
		_ = p.client.Close()
		p.client = nil
	}
}

type dasusmPayload struct {
	NodeCommon struct {
		Username  string `json:"Username"`
		IPv4      string `json:"IPv4"`
		Port      string `json:"Port"`
		Protocol  string `json:"Protocol"`
		SSOToken  string `json:"SSOToken"`
		AssetName string `json:"AssetName"`
	} `json:"NODE_COMMON"`
}

func LatestDASUSMSession() (xsh.Session, error) {
	sessions, err := LatestDASUSMSessions()
	if err != nil {
		return xsh.Session{}, err
	}
	if len(sessions) == 0 {
		return xsh.Session{}, errors.New("no live Xshell SSH URL found")
	}
	return sessions[0], nil
}

func LatestDASUSMSessions() ([]xsh.Session, error) {
	return latestDASUSMSessions(envBool("OPSERA_SSO_CORE_ONLY"))
}

func latestDASUSMSessions(coreOnly bool) ([]xsh.Session, error) {
	processes, err := latestXshellProcessSnapshots()
	if err != nil {
		return nil, err
	}
	sessions := sessionsFromXshellProcesses(processes, coreOnly)
	if len(sessions) == 0 {
		return nil, errors.New("no live Xshell SSH URL found")
	}
	return sessions, nil
}

func latestXshellCoreSession() (xsh.Session, error) {
	return LatestDASUSMSession()
}

type xshellProcessSnapshot struct {
	ProcessID   int    `json:"ProcessId"`
	Name        string `json:"Name"`
	CommandLine string `json:"CommandLine"`
}

func latestXshellProcessSnapshots() ([]xshellProcessSnapshot, error) {
	command := `$ErrorActionPreference = 'Stop'; $rows = Get-CimInstance Win32_Process | Where-Object { $_.Name -eq 'Xshell.exe' -or $_.Name -eq 'XshellCore.exe' } | Sort-Object ProcessId -Descending | Select-Object ProcessId,Name,CommandLine; if ($null -eq $rows) { '[]' } else { $rows | ConvertTo-Json -Compress }`
	out, err := exec.Command("powershell.exe", "-NoProfile", "-Command", command).Output()
	if err != nil {
		return nil, err
	}
	return parseXshellProcessSnapshots(out)
}

func parseXshellProcessSnapshots(data []byte) ([]xshellProcessSnapshot, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return nil, nil
	}
	if data[0] == '[' {
		var processes []xshellProcessSnapshot
		if err := json.Unmarshal(data, &processes); err != nil {
			return nil, err
		}
		return processes, nil
	}
	var process xshellProcessSnapshot
	if err := json.Unmarshal(data, &process); err != nil {
		return nil, err
	}
	return []xshellProcessSnapshot{process}, nil
}

func sessionsFromXshellProcesses(processes []xshellProcessSnapshot, coreOnly bool) []xsh.Session {
	sort.SliceStable(processes, func(i, j int) bool {
		leftPriority := xshellProcessPriority(processes[i].Name)
		rightPriority := xshellProcessPriority(processes[j].Name)
		if leftPriority != rightPriority {
			return leftPriority < rightPriority
		}
		return processes[i].ProcessID > processes[j].ProcessID
	})

	sessions := []xsh.Session{}
	seen := map[string]bool{}
	for _, process := range processes {
		name := strings.TrimSpace(process.Name)
		isCore := strings.EqualFold(name, "XshellCore.exe")
		if coreOnly && !isCore {
			continue
		}
		if !isCore && !strings.EqualFold(name, "Xshell.exe") {
			continue
		}
		session, err := sessionFromSSHURLInText(process.CommandLine)
		if err != nil {
			continue
		}
		key := sessionCacheKey(session)
		if seen[key] {
			continue
		}
		seen[key] = true
		sessions = append(sessions, session)
	}
	return sessions
}

func xshellProcessPriority(name string) int {
	if strings.EqualFold(strings.TrimSpace(name), "XshellCore.exe") {
		return 0
	}
	if strings.EqualFold(strings.TrimSpace(name), "Xshell.exe") {
		return 1
	}
	return 2
}

func sessionCacheKey(session xsh.Session) string {
	sum := sha256.Sum256([]byte(session.Password))
	return fmt.Sprintf("%s\x00%d\x00%s\x00%s", session.Host, session.Port, session.UserName, hex.EncodeToString(sum[:]))
}

func envBool(name string) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	return value == "1" || value == "true" || value == "yes" || value == "on"
}

func sessionFailure(session xsh.Session, err error) string {
	user := strings.TrimSpace(session.UserName)
	if user == "" {
		user = "unknown"
	}
	host := strings.TrimSpace(session.Host)
	if host == "" {
		host = "unknown"
	}
	port := session.Port
	if port <= 0 {
		port = 22
	}
	return fmt.Sprintf("%s@%s:%d: %v", user, host, port, err)
}

func latestXshellCoreSessionsFromLines(lines []string) []xsh.Session {
	processes := make([]xshellProcessSnapshot, 0, len(lines))
	for i, line := range lines {
		processes = append(processes, xshellProcessSnapshot{
			ProcessID:   len(lines) - i,
			Name:        "XshellCore.exe",
			CommandLine: line,
		})
	}
	return sessionsFromXshellProcesses(processes, false)
}

func latestXshellCoreSessionFromLines(lines []string) (xsh.Session, error) {
	sessions := latestXshellCoreSessionsFromLines(lines)
	if len(sessions) > 0 {
		return sessions[0], nil
	}
	return xsh.Session{}, errors.New("no live Xshell SSH URL found")
}

func sessionFromSSHURLInText(text string) (xsh.Session, error) {
	idx := strings.Index(text, "ssh://")
	if idx < 0 {
		return xsh.Session{}, errors.New("ssh url not found")
	}
	raw := strings.TrimSpace(text[idx:])
	if quote := strings.IndexAny(raw, "\"' \r\n\t"); quote >= 0 {
		raw = raw[:quote]
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return xsh.Session{}, err
	}
	if parsed.Scheme != "ssh" || parsed.User == nil {
		return xsh.Session{}, errors.New("invalid ssh url")
	}
	password, ok := parsed.User.Password()
	if !ok {
		return xsh.Session{}, errors.New("ssh url has no password")
	}
	port := 22
	if parsed.Port() != "" {
		parsedPort, err := strconv.Atoi(parsed.Port())
		if err != nil || parsedPort <= 0 {
			return xsh.Session{}, errors.New("invalid ssh url port")
		}
		port = parsedPort
	}
	return xsh.Session{
		Host:     parsed.Hostname(),
		Port:     port,
		UserName: parsed.User.Username(),
		Password: password,
	}, nil
}

func parseDASUSMLog(path string) (xsh.Session, error) {
	file, err := os.Open(path)
	if err != nil {
		return xsh.Session{}, err
	}
	defer file.Close()
	lines := []string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return xsh.Session{}, err
	}
	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
		if !strings.Contains(line, "load.go:91:") || !strings.Contains(line, `"NODE_COMMON"`) {
			continue
		}
		idx := strings.Index(line, "{")
		if idx < 0 {
			continue
		}
		var payload dasusmPayload
		if err := json.Unmarshal([]byte(line[idx:]), &payload); err != nil {
			continue
		}
		common := payload.NodeCommon
		if !strings.EqualFold(strings.TrimSpace(common.Protocol), "SSH") {
			continue
		}
		port, err := strconv.Atoi(strings.TrimSpace(common.Port))
		if err != nil || port <= 0 {
			continue
		}
		if strings.TrimSpace(common.IPv4) == "" || strings.TrimSpace(common.Username) == "" || strings.TrimSpace(common.SSOToken) == "" {
			continue
		}
		return xsh.Session{
			Host:     strings.TrimSpace(common.IPv4),
			Port:     port,
			UserName: strings.TrimSpace(common.Username),
			Password: strings.TrimSpace(common.SSOToken),
		}, nil
	}
	return xsh.Session{}, errors.New("no usable DASUSM SSH SSO record in " + path)
}

func runServerCommandViaAPI(serverRef string, command string) (model.Command, error) {
	body, err := json.Marshal(map[string]string{
		"server":  serverRef,
		"command": command,
	})
	if err != nil {
		return model.Command{}, err
	}
	client := http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Post("http://127.0.0.1:18741/command/run", "application/json", bytes.NewReader(body))
	if err != nil {
		return model.Command{}, err
	}
	defer resp.Body.Close()
	var item model.Command
	if err := json.NewDecoder(resp.Body).Decode(&item); err != nil {
		return model.Command{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return item, fmt.Errorf("opsera api returned %s", resp.Status)
	}
	if item.Status == model.CommandStatusFailed {
		return item, errors.New(item.Error)
	}
	return item, nil
}

func OpenKeepAlive(xshPath string) (*ssh.Client, error) {
	session, err := LatestSession(xshPath)
	if err != nil {
		return nil, err
	}
	return dialSession(session)
}

func serverSession(serverRef string) (xsh.Session, error) {
	dataDir, err := resolveDataDir()
	if err != nil {
		return xsh.Session{}, err
	}
	store, err := config.NewStore(filepath.Join(dataDir, "config"))
	if err != nil {
		return xsh.Session{}, err
	}
	ref := strings.ToLower(strings.TrimSpace(serverRef))
	var server model.Server
	for _, item := range store.Snapshot().Servers {
		if strings.ToLower(item.ID) == ref ||
			strings.ToLower(item.Name) == ref ||
			strings.ToLower(item.Host) == ref ||
			strings.Contains(strings.ToLower(item.Name), ref) {
			server = item
			break
		}
	}
	if server.ID == "" {
		return xsh.Session{}, fmt.Errorf("server not found: %s", serverRef)
	}
	if server.Mode != "" && server.Mode != model.ConnectionModeDirectSSH {
		return xsh.Session{}, fmt.Errorf("server %s is not a direct SSH server", server.Name)
	}
	var credential model.Credential
	for _, item := range store.Snapshot().Credentials {
		if item.ID == server.CredentialRef {
			credential = item
			break
		}
	}
	if credential.ID == "" {
		return xsh.Session{}, fmt.Errorf("credential not found for server: %s", server.Name)
	}
	password, err := crypto.NewVault(dataDir).Decrypt(credential.SecretCipher)
	if err != nil {
		return xsh.Session{}, err
	}
	port := server.Port
	if port <= 0 {
		port = 22
	}
	return xsh.Session{
		Host:     server.Host,
		Port:     port,
		UserName: credential.Username,
		Password: password,
	}, nil
}

func dialSession(session xsh.Session) (*ssh.Client, error) {
	config := &ssh.ClientConfig{
		User: session.UserName,
		Auth: []ssh.AuthMethod{
			ssh.Password(session.Password),
			ssh.KeyboardInteractive(func(user, instruction string, questions []string, echos []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range answers {
					answers[i] = session.Password
				}
				return answers, nil
			}),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         8 * time.Second,
		Config: ssh.Config{
			KeyExchanges: []string{
				"diffie-hellman-group14-sha1",
				"diffie-hellman-group1-sha1",
				"diffie-hellman-group14-sha256",
				"curve25519-sha256",
				"curve25519-sha256@libssh.org",
			},
		},
		HostKeyAlgorithms: []string{ssh.KeyAlgoRSA, ssh.KeyAlgoDSA},
	}
	client, err := ssh.Dial("tcp", fmt.Sprintf("%s:%d", session.Host, session.Port), config)
	if err == nil {
		go keepAlive(client)
	}
	return client, err
}

func sshAlive(client *ssh.Client) bool {
	return checkClient(client) == nil
}

func checkClient(client *ssh.Client) error {
	if client == nil {
		return errors.New("ssh client is nil")
	}
	_, _, err := client.SendRequest("keepalive@opsera", true, nil)
	return err
}

func keepAlive(client *ssh.Client) {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if _, _, err := client.SendRequest("keepalive@opsera", true, nil); err != nil {
			return
		}
	}
}

func runFile(args []string) (bool, error) {
	if len(args) == 0 {
		return true, errors.New("usage: opsera file upload|upload-large|download [--xsh <path>] [--sso] <local> <remote>")
	}
	if args[0] == "upload-large" {
		return runUploadLarge(args[1:])
	}
	if args[0] == "download" {
		return runDownload(args[1:])
	}
	if args[0] != "upload" {
		return true, errors.New("usage: opsera file upload [--server <name|host|id>] [--xsh <path>] [--sso] <local> <remote>")
	}
	serverRef := ""
	xshPath := ""
	useSSO := false
	fileArgs := args[1:]
	for len(fileArgs) > 0 {
		if fileArgs[0] == "--server" {
			if len(fileArgs) < 2 {
				return true, errors.New("--server requires a value")
			}
			serverRef = fileArgs[1]
			fileArgs = fileArgs[2:]
			continue
		}
		if fileArgs[0] == "--xsh" {
			if len(fileArgs) < 2 {
				return true, errors.New("--xsh requires a value")
			}
			xshPath = fileArgs[1]
			fileArgs = fileArgs[2:]
			continue
		}
		if fileArgs[0] == "--sso" || fileArgs[0] == "--dasusm" {
			useSSO = true
			fileArgs = fileArgs[1:]
			continue
		}
		break
	}
	if len(fileArgs) != 2 {
		return true, errors.New("usage: opsera file upload [--server <name|host|id>] [--xsh <path>] [--sso] <local> <remote>")
	}
	result := map[string]any{
		"local":  fileArgs[0],
		"remote": fileArgs[1],
	}
	dataDir, _ := resolveDataDir()
	var err error
	if strings.TrimSpace(serverRef) != "" {
		err = uploadServerFile(serverRef, fileArgs[0], fileArgs[1])
	} else if useSSO {
		err = uploadDASUSMFile(fileArgs[0], fileArgs[1])
	} else {
		err = uploadFile(xshPath, fileArgs[0], fileArgs[1])
	}
	if err != nil {
		result["status"] = "failed"
		result["error"] = err.Error()
		_ = events.Write(dataDir, events.Event{
			Type:    "upload",
			Command: "upload " + fileArgs[0] + " " + fileArgs[1],
			Error:   err.Error(),
			Status:  "failed",
		})
		_ = json.NewEncoder(os.Stdout).Encode(result)
		return true, err
	}
	result["status"] = "done"
	_ = events.Write(dataDir, events.Event{
		Type:    "upload",
		Command: "upload " + fileArgs[0] + " " + fileArgs[1],
		Output:  "uploaded",
		Status:  "done",
	})
	return true, writeJSON(result)
}

func runDownload(args []string) (bool, error) {
	xshPath := ""
	useSSO := false
	fileArgs := args
	for len(fileArgs) > 0 {
		if fileArgs[0] == "--xsh" {
			if len(fileArgs) < 2 {
				return true, errors.New("--xsh requires a value")
			}
			xshPath = fileArgs[1]
			fileArgs = fileArgs[2:]
			continue
		}
		if fileArgs[0] == "--sso" || fileArgs[0] == "--dasusm" {
			useSSO = true
			fileArgs = fileArgs[1:]
			continue
		}
		break
	}
	if len(fileArgs) != 2 {
		return true, errors.New("usage: opsera file download [--xsh <path>] [--sso] <remote> <local>")
	}
	result := map[string]any{
		"remote": fileArgs[0],
		"local":  fileArgs[1],
	}
	dataDir, _ := resolveDataDir()
	var err error
	if useSSO {
		err = downloadDASUSMFile(fileArgs[0], fileArgs[1])
	} else {
		err = downloadFile(xshPath, fileArgs[0], fileArgs[1])
	}
	if err != nil {
		result["status"] = "failed"
		result["error"] = err.Error()
		_ = events.Write(dataDir, events.Event{
			Type:    "download",
			Command: "download " + fileArgs[0] + " " + fileArgs[1],
			Error:   err.Error(),
			Status:  "failed",
		})
		_ = json.NewEncoder(os.Stdout).Encode(result)
		return true, err
	}
	result["status"] = "done"
	_ = events.Write(dataDir, events.Event{
		Type:    "download",
		Command: "download " + fileArgs[0] + " " + fileArgs[1],
		Output:  "downloaded",
		Status:  "done",
	})
	return true, writeJSON(result)
}

func runUploadLarge(args []string) (bool, error) {
	xshPath := ""
	useSSO := false
	chunkSize := int64(512 * 1024 * 1024)
	fileArgs := args
	for len(fileArgs) > 0 {
		if fileArgs[0] == "--xsh" {
			if len(fileArgs) < 2 {
				return true, errors.New("--xsh requires a value")
			}
			xshPath = fileArgs[1]
			fileArgs = fileArgs[2:]
			continue
		}
		if fileArgs[0] == "--sso" || fileArgs[0] == "--dasusm" {
			useSSO = true
			fileArgs = fileArgs[1:]
			continue
		}
		if fileArgs[0] == "--chunk-mb" {
			if len(fileArgs) < 2 {
				return true, errors.New("--chunk-mb requires a value")
			}
			mb, err := strconv.Atoi(fileArgs[1])
			if err != nil || mb <= 0 {
				return true, errors.New("invalid --chunk-mb")
			}
			chunkSize = int64(mb) * 1024 * 1024
			fileArgs = fileArgs[2:]
			continue
		}
		break
	}
	if len(fileArgs) != 2 {
		return true, errors.New("usage: opsera file upload-large [--xsh <path>] [--sso] [--chunk-mb 512] <local> <remote>")
	}
	result := map[string]any{"local": fileArgs[0], "remote": fileArgs[1], "chunkBytes": chunkSize}
	dataDir, _ := resolveDataDir()
	var err error
	if useSSO {
		err = uploadLargeDASUSMFile(fileArgs[0], fileArgs[1], chunkSize)
	} else {
		err = uploadLargeFile(xshPath, fileArgs[0], fileArgs[1], chunkSize)
	}
	if err != nil {
		result["status"] = "failed"
		result["error"] = err.Error()
		_ = events.Write(dataDir, events.Event{Type: "upload-large", Command: "upload-large " + fileArgs[0] + " " + fileArgs[1], Error: err.Error(), Status: "failed"})
		_ = json.NewEncoder(os.Stdout).Encode(result)
		return true, err
	}
	result["status"] = "done"
	_ = events.Write(dataDir, events.Event{Type: "upload-large", Command: "upload-large " + fileArgs[0] + " " + fileArgs[1], Output: "uploaded", Status: "done"})
	return true, writeJSON(result)
}

func uploadFile(xshPath, localPath, remotePath string) error {
	session, err := LatestSession(xshPath)
	if err != nil {
		return err
	}
	return uploadSessionFile(session, localPath, remotePath)
}

func uploadServerFile(serverRef, localPath, remotePath string) error {
	session, err := serverSession(serverRef)
	if err != nil {
		return err
	}
	return uploadSessionFile(session, localPath, remotePath)
}

func uploadDASUSMFile(localPath, remotePath string) error {
	if _, handled, err := callSSOAgent("POST", "/sso/file/upload", map[string]string{
		"local":  localPath,
		"remote": remotePath,
	}, 10*time.Minute); handled {
		return err
	}
	sessions, err := LatestDASUSMSessions()
	if err != nil {
		return err
	}
	return tryDASUSMSessions(sessions, func(session xsh.Session) error {
		return uploadSessionFile(session, localPath, remotePath)
	})
}

func uploadSessionFile(session xsh.Session, localPath, remotePath string) error {
	client, err := dialSession(session)
	if err != nil {
		return err
	}
	defer client.Close()
	return uploadWithClient(client, localPath, remotePath)
}

func uploadWithClient(client *ssh.Client, localPath, remotePath string) error {
	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		return err
	}
	defer sftpClient.Close()
	src, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer src.Close()
	if err := sftpClient.MkdirAll(filepath.Dir(remotePath)); err != nil {
		return err
	}
	dst, err := sftpClient.Create(remotePath)
	if err != nil {
		return err
	}
	defer dst.Close()
	_, err = dst.ReadFrom(src)
	return err
}

func uploadLargeFile(xshPath, localPath, remotePath string, chunkSize int64) error {
	session, err := LatestSession(xshPath)
	if err != nil {
		return err
	}
	return uploadLargeSessionFile(session, localPath, remotePath, chunkSize)
}

func uploadLargeDASUSMFile(localPath, remotePath string, chunkSize int64) error {
	if _, handled, err := callSSOAgent("POST", "/sso/file/upload-large", map[string]any{
		"local":      localPath,
		"remote":     remotePath,
		"chunkBytes": chunkSize,
	}, 24*time.Hour); handled {
		return err
	}
	sessions, err := LatestDASUSMSessions()
	if err != nil {
		return err
	}
	return tryDASUSMSessions(sessions, func(session xsh.Session) error {
		return uploadLargeSessionFile(session, localPath, remotePath, chunkSize)
	})
}

func uploadLargeSessionFile(session xsh.Session, localPath, remotePath string, chunkSize int64) error {
	client, err := dialSession(session)
	if err != nil {
		return err
	}
	defer client.Close()
	return uploadLargeWithClient(client, localPath, remotePath, chunkSize)
}

func uploadLargeWithClient(client *ssh.Client, localPath, remotePath string, chunkSize int64) error {
	info, err := os.Stat(localPath)
	if err != nil {
		return err
	}
	sha, err := fileSHA256(localPath)
	if err != nil {
		return err
	}
	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		return err
	}
	defer sftpClient.Close()

	partsDir := remotePath + ".parts"
	if err := sftpClient.MkdirAll(partsDir); err != nil {
		return err
	}
	src, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer src.Close()
	total := info.Size()
	chunks := int((total + chunkSize - 1) / chunkSize)
	buf := make([]byte, 2*1024*1024)
	for i := 0; i < chunks; i++ {
		partSize := chunkSize
		if remain := total - int64(i)*chunkSize; remain < partSize {
			partSize = remain
		}
		partPath := fmt.Sprintf("%s/part-%06d", partsDir, i)
		if st, err := sftpClient.Stat(partPath); err == nil && st.Size() == partSize {
			continue
		}
		if _, err := src.Seek(int64(i)*chunkSize, io.SeekStart); err != nil {
			return err
		}
		dst, err := sftpClient.Create(partPath + ".tmp")
		if err != nil {
			return err
		}
		if _, err := io.CopyBuffer(dst, io.LimitReader(src, partSize), buf); err != nil {
			_ = dst.Close()
			return err
		}
		if err := dst.Close(); err != nil {
			return err
		}
		if err := sftpClient.Rename(partPath+".tmp", partPath); err != nil {
			return err
		}
	}
	merge := fmt.Sprintf("cat %s/part-* > %s && sha256sum %s", shellQuote(partsDir), shellQuote(remotePath), shellQuote(remotePath))
	out, err := remoteRun(client, merge)
	if err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	if !strings.HasPrefix(strings.TrimSpace(out), sha) {
		return fmt.Errorf("sha256 mismatch: local %s remote %s", sha, strings.TrimSpace(out))
	}
	return nil
}

func remoteRun(client *ssh.Client, command string) (string, error) {
	return runWithClient(client, command)
}

func runWithClient(client *ssh.Client, command string) (string, error) {
	session, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()
	out, err := session.CombinedOutput(command)
	return string(out), err
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func downloadFile(xshPath, remotePath, localPath string) error {
	session, err := LatestSession(xshPath)
	if err != nil {
		return err
	}
	return downloadSessionFile(session, remotePath, localPath)
}

func downloadDASUSMFile(remotePath, localPath string) error {
	if _, handled, err := callSSOAgent("POST", "/sso/file/download", map[string]string{
		"remote": remotePath,
		"local":  localPath,
	}, 10*time.Minute); handled {
		return err
	}
	sessions, err := LatestDASUSMSessions()
	if err != nil {
		return err
	}
	return tryDASUSMSessions(sessions, func(session xsh.Session) error {
		return downloadSessionFile(session, remotePath, localPath)
	})
}

func downloadSessionFile(session xsh.Session, remotePath, localPath string) error {
	client, err := dialSession(session)
	if err != nil {
		return err
	}
	defer client.Close()
	return downloadWithClient(client, remotePath, localPath)
}

func downloadWithClient(client *ssh.Client, remotePath, localPath string) error {
	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		return err
	}
	defer sftpClient.Close()
	src, err := sftpClient.Open(remotePath)
	if err != nil {
		return err
	}
	defer src.Close()
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return err
	}
	dst, err := os.Create(localPath)
	if err != nil {
		return err
	}
	defer dst.Close()
	_, err = dst.ReadFrom(src)
	return err
}

func tryDASUSMSessions(sessions []xsh.Session, run func(xsh.Session) error) error {
	failures := []string{}
	failed := map[string]bool{}
	for _, session := range sessions {
		key := sessionCacheKey(session)
		if failed[key] {
			continue
		}
		if err := run(session); err != nil {
			failed[key] = true
			failures = append(failures, sessionFailure(session, err))
			continue
		}
		return nil
	}
	if len(failures) == 0 {
		return errors.New("no live Xshell SSH URL found")
	}
	return errors.New("all live Xshell SSH URLs failed: " + strings.Join(failures, "; "))
}

func LatestSession(explicitPath string) (xsh.Session, error) {
	if strings.TrimSpace(explicitPath) != "" {
		return xsh.Parse(strings.Trim(strings.TrimSpace(explicitPath), `"`))
	}
	candidates := []string{}
	seen := map[string]bool{}
	dataDir, err := resolveDataDir()
	if err == nil {
		logStore, err := logs.NewStore(filepath.Join(dataDir, "logs"))
		if err == nil {
			entries, err := logStore.ReadLatest(300)
			if err == nil {
				for i := len(entries) - 1; i >= 0; i-- {
					if entries[i].Source != "argv" {
						continue
					}
					path := xshPathPattern.FindString(entries[i].Message)
					if path == "" || seen[path] {
						continue
					}
					seen[path] = true
					candidates = append(candidates, path)
				}
			}
		}
	}
	pattern := filepath.Join(os.Getenv("APPDATA"), "NetSarang", "Xshell", "Sessions", "*.xsh")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return xsh.Session{}, err
	}
	sortByModTimeDesc(matches)
	for _, path := range matches {
		if !seen[path] {
			seen[path] = true
			candidates = append(candidates, path)
		}
	}
	if len(candidates) == 0 {
		return xsh.Session{}, errors.New("no xshell session file found")
	}
	var first xsh.Session
	for _, path := range candidates {
		session, err := xsh.Parse(path)
		if err != nil {
			continue
		}
		if first.Path == "" {
			first = session
		}
		if tcpAlive(session.Host, session.Port) {
			return session, nil
		}
	}
	if first.Path != "" {
		return xsh.Session{}, fmt.Errorf("no active ssh tunnel found; last session is %s:%d from %s", first.Host, first.Port, first.Path)
	}
	return xsh.Session{}, errors.New("no readable xshell session file found")
}

func sortByModTimeDesc(paths []string) {
	type item struct {
		path string
		mod  time.Time
	}
	items := make([]item, 0, len(paths))
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		items = append(items, item{path: path, mod: info.ModTime()})
	}
	for i := 0; i < len(items); i++ {
		for j := i + 1; j < len(items); j++ {
			if items[j].mod.After(items[i].mod) {
				items[i], items[j] = items[j], items[i]
			}
		}
	}
	for i := range items {
		paths[i] = items[i].path
	}
}

func tcpAlive(host string, port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func resolveDataDir() (string, error) {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base = os.Getenv("HOME")
	}
	if base == "" {
		return "", errors.New("no user data directory available")
	}
	root := filepath.Join(base, "Opsera")
	return root, os.MkdirAll(root, 0o755)
}
