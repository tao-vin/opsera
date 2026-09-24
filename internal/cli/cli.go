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
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/tao-vin/opsera/internal/config"
	"github.com/tao-vin/opsera/internal/crypto"
	"github.com/tao-vin/opsera/internal/events"
	"github.com/tao-vin/opsera/internal/logs"
	"github.com/tao-vin/opsera/internal/model"
	"github.com/tao-vin/opsera/internal/session"
	"github.com/tao-vin/opsera/internal/version"
	"github.com/tao-vin/opsera/internal/xsh"
)

var xshPathPattern = regexp.MustCompile(`[A-Za-z]:\\[^"]+\.xsh`)

const ssoAgentBaseURL = "http://127.0.0.1:18742"

var (
	cliUser32                  = syscall.NewLazyDLL("user32.dll")
	cliProcEnumWindows         = cliUser32.NewProc("EnumWindows")
	cliProcGetWindowTextW      = cliUser32.NewProc("GetWindowTextW")
	cliProcGetWindowTextLength = cliUser32.NewProc("GetWindowTextLengthW")
	cliProcIsWindowVisible     = cliUser32.NewProc("IsWindowVisible")
	probeTunnelSession         = defaultProbeTunnelSession
	latestSessionCacheMu       sync.Mutex
	latestSessionCache         xsh.Session
)

func TryRun(args []string) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	if isCLIHelpArg(args[0]) {
		fmt.Fprintln(os.Stdout, cliUsage())
		return true, nil
	}
	if isCLIVersionArg(args[0]) {
		return true, writeJSON(version.Current())
	}
	if args[0] == "sso" {
		return runSSO(args[1:])
	}
	if args[0] == "file" {
		return runFile(args[1:])
	}
	if args[0] == "server" {
		return runServer(args[1:])
	}
	if args[0] == "session" {
		return runSession(args[1:])
	}
	if args[0] == "xshell" {
		return runXshell(args[1:])
	}
	if args[0] == "relay" {
		return runRelay(args[1:])
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
		if runErr != nil {
			if apiItem, err := runOpenSessionCommandViaAPI(serverRef, command); err == nil {
				_ = logStore.Append(model.LogLevelInfo, "command", "done via opsera open session: "+command, apiItem.ID)
				return true, writeJSON(apiItem)
			}
		}
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
		"  opsera session list",
		"  opsera session run [--target <target>] <shell-command>",
		"  opsera session diagnose [target]",
		"  opsera session simulate-launch <launch-arg...>",
		"  opsera server list [query]",
		"  opsera server search <project-or-keyword>",
		"  opsera xshell keepalive",
		"  opsera xshell keepalive status",
		"  opsera relay status <code> <password> [relay-host]",
		"  opsera relay exec <code> <password> <powershell-command>",
		"  opsera relay upload <code> <password> <local> <remote>",
		"  opsera relay deploy <code> <password> <local-exe> [remote-exe]",
		"  opsera sso status",
		"  opsera sso attach [--core-only] [--window-keepalive] [--no-browser-keepalive]",
		"  opsera sso agent [--core-only] [--window-keepalive] [--no-browser-keepalive]",
		"  opsera file upload [--server <name|host|id>] [--xsh <path>] [--sso] <local> <remote>",
		"  opsera file upload-large [--server <name|host|id>] [--xsh <path>] [--sso] [--chunk-mb 512] <local> <remote>",
		"  opsera file download [--xsh <path>] [--sso] <remote> <local>",
	}, "\n")
}

func runXshell(args []string) (bool, error) {
	if len(args) == 0 {
		return true, errors.New("usage: opsera xshell keepalive [status]")
	}
	switch args[0] {
	case "keepalive":
		return runXshellKeepAlive(args[1:])
	default:
		return true, errors.New("usage: opsera xshell keepalive [status]")
	}
}

func runServer(args []string) (bool, error) {
	if len(args) == 0 {
		return true, errors.New("usage: opsera server list [query] | opsera server search <project-or-keyword>")
	}
	switch args[0] {
	case "list", "search":
		query := strings.TrimSpace(strings.Join(args[1:], " "))
		if args[0] == "search" && query == "" {
			return true, errors.New("usage: opsera server search <project-or-keyword>")
		}
		dataDir, err := resolveDataDir()
		if err != nil {
			return true, err
		}
		store, err := config.NewStore(filepath.Join(dataDir, "config"))
		if err != nil {
			return true, err
		}
		servers := store.FindServers(query)
		items := make([]serverListItem, 0, len(servers))
		for _, server := range servers {
			items = append(items, serverListItem{
				ID:          server.ID,
				Name:        server.Name,
				Host:        server.Host,
				Port:        server.Port,
				Mode:        server.Mode,
				MachineType: server.MachineType,
				MachineCode: server.MachineCode,
				Notes:       server.Notes,
				Tags:        append([]string(nil), server.Tags...),
			})
		}
		return true, writeJSON(items)
	default:
		return true, errors.New("usage: opsera server list [query] | opsera server search <project-or-keyword>")
	}
}

func runSession(args []string) (bool, error) {
	if len(args) == 0 {
		return true, errors.New("usage: opsera session list | opsera session run [--target <target>] <shell-command> | opsera session diagnose [target] | opsera session simulate-launch <launch-arg...>")
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return true, errors.New("usage: opsera session list")
		}
		items, err := listOpenSessionsViaAPI()
		if err != nil {
			return true, err
		}
		return true, writeJSON(items)
	case "run":
		target := ""
		commandArgs := args[1:]
		for len(commandArgs) >= 2 {
			if commandArgs[0] == "--target" {
				target = commandArgs[1]
				commandArgs = commandArgs[2:]
				continue
			}
			break
		}
		command := strings.TrimSpace(strings.Join(commandArgs, " "))
		if command == "" {
			return true, errors.New("usage: opsera session run [--target <target>] <shell-command>")
		}
		item, err := runOpenSessionCommandViaAPI(target, command)
		if err != nil {
			return true, err
		}
		return true, writeJSON(item)
	case "diagnose":
		target := strings.TrimSpace(strings.Join(args[1:], " "))
		result, err := diagnoseOpenSessionLaunch(target)
		if err != nil {
			_ = writeJSON(result)
			return true, err
		}
		return true, writeJSON(result)
	case "simulate-launch":
		if len(args) < 2 {
			return true, errors.New("usage: opsera session simulate-launch <launch-arg...>")
		}
		result, err := simulateOpenSessionLaunch(args[1:])
		if err != nil {
			_ = writeJSON(result)
			return true, err
		}
		return true, writeJSON(result)
	default:
		return true, errors.New("usage: opsera session list | opsera session run [--target <target>] <shell-command> | opsera session diagnose [target] | opsera session simulate-launch <launch-arg...>")
	}
}

type serverListItem struct {
	ID          string               `json:"id"`
	Name        string               `json:"name"`
	Host        string               `json:"host,omitempty"`
	Port        int                  `json:"port,omitempty"`
	Mode        model.ConnectionMode `json:"mode,omitempty"`
	MachineType model.MachineType    `json:"machineType,omitempty"`
	MachineCode string               `json:"machineCode,omitempty"`
	Notes       string               `json:"notes,omitempty"`
	Tags        []string             `json:"tags,omitempty"`
}

type launchDiagnostic struct {
	DesktopAPI       bool              `json:"desktopAPI"`
	DesktopAPIError  string            `json:"desktopAPIError,omitempty"`
	Target           string            `json:"target,omitempty"`
	ParsedTarget     string            `json:"parsedTarget,omitempty"`
	ServerPresent    bool              `json:"serverPresent"`
	SessionPresent   bool              `json:"sessionPresent"`
	ServerCount      int               `json:"serverCount"`
	SessionCount     int               `json:"sessionCount"`
	MatchedServers   []serverListItem  `json:"matchedServers,omitempty"`
	MatchedSessions  []model.Session   `json:"matchedSessions,omitempty"`
	RecentLaunchArgs []launchArgRecord `json:"recentLaunchArgs,omitempty"`
	Status           string            `json:"status"`
	Diagnosis        string            `json:"diagnosis"`
}

type launchArgRecord struct {
	Timestamp string   `json:"timestamp"`
	Level     string   `json:"level"`
	Message   string   `json:"message"`
	Args      []string `json:"args,omitempty"`
	Target    string   `json:"target,omitempty"`
}

func diagnoseOpenSessionLaunch(target string) (launchDiagnostic, error) {
	dataDir, err := resolveDataDir()
	if err != nil {
		return launchDiagnostic{}, err
	}
	result := launchDiagnostic{Target: strings.TrimSpace(target)}
	logStore, err := logs.NewStore(filepath.Join(dataDir, "logs"))
	if err == nil {
		entries, _ := logStore.ReadLatest(300)
		result.RecentLaunchArgs = recentLaunchArgRecords(entries, 10)
	}
	if result.Target == "" {
		for _, record := range result.RecentLaunchArgs {
			if strings.TrimSpace(record.Target) != "" {
				result.Target = record.Target
				break
			}
		}
	}
	servers, serverErr := listDesktopServersViaAPI()
	sessions, sessionErr := listOpenSessionsViaAPI()
	if serverErr == nil || sessionErr == nil {
		result.DesktopAPI = true
	} else {
		result.DesktopAPIError = serverErr.Error()
		if sessionErr != nil && sessionErr.Error() != result.DesktopAPIError {
			result.DesktopAPIError += "; " + sessionErr.Error()
		}
	}
	if serverErr == nil {
		result.ServerCount = len(servers)
		result.MatchedServers = matchingServerItems(servers, result.Target)
		result.ServerPresent = len(result.MatchedServers) > 0
	}
	if sessionErr == nil {
		result.SessionCount = len(sessions)
		result.MatchedSessions = matchingSessions(sessions, result.Target)
		result.SessionPresent = len(result.MatchedSessions) > 0
	}
	result.Status, result.Diagnosis = launchDiagnosticStatus(result)
	if result.Status != "ok" {
		return result, errors.New(result.Diagnosis)
	}
	return result, nil
}

func simulateOpenSessionLaunch(args []string) (launchDiagnostic, error) {
	target := session.LaunchTarget(args)
	result := launchDiagnostic{Target: target, ParsedTarget: target}
	if target == "" {
		result.Status = "failed"
		result.Diagnosis = "launch args did not contain a target"
		return result, errors.New(result.Diagnosis)
	}
	body, err := json.Marshal(map[string][]string{"args": args})
	if err != nil {
		return result, err
	}
	client := http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post("http://127.0.0.1:18741/launch", "application/json", bytes.NewReader(body))
	if err != nil {
		result.DesktopAPIError = err.Error()
		result.Status = "failed"
		result.Diagnosis = "desktop Opsera API is not reachable"
		return result, errors.New(result.Diagnosis + ": " + err.Error())
	}
	defer resp.Body.Close()
	result.DesktopAPI = true
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		result.Status = "failed"
		result.Diagnosis = "desktop Opsera /launch returned " + resp.Status
		return result, errors.New(result.Diagnosis)
	}
	diagnostic, err := diagnoseOpenSessionLaunch(target)
	diagnostic.ParsedTarget = target
	if err != nil {
		return diagnostic, err
	}
	return diagnostic, nil
}

func recentLaunchArgRecords(entries []model.LogEntry, limit int) []launchArgRecord {
	records := []launchArgRecord{}
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		if entry.Source != "argv" {
			continue
		}
		record := launchArgRecord{
			Timestamp: entry.Timestamp,
			Level:     string(entry.Level),
			Message:   entry.Message,
		}
		var args []string
		if err := json.Unmarshal([]byte(entry.Message), &args); err == nil {
			record.Args = args
			record.Target = session.LaunchTarget(args)
		} else {
			record.Target = session.LaunchTarget([]string{entry.Message})
		}
		records = append(records, record)
		if len(records) >= limit {
			break
		}
	}
	return records
}

func matchingServerItems(servers []model.Server, target string) []serverListItem {
	target = strings.ToLower(strings.TrimSpace(target))
	if target == "" {
		return nil
	}
	var matches []serverListItem
	for _, server := range servers {
		if strings.EqualFold(server.ID, target) || strings.EqualFold(server.Name, target) || strings.EqualFold(server.Host, target) {
			matches = append(matches, serverListItem{
				ID:          server.ID,
				Name:        server.Name,
				Host:        server.Host,
				Port:        server.Port,
				Mode:        server.Mode,
				MachineType: server.MachineType,
				MachineCode: server.MachineCode,
				Notes:       server.Notes,
				Tags:        append([]string(nil), server.Tags...),
			})
		}
	}
	return matches
}

func matchingSessions(sessions []model.Session, target string) []model.Session {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil
	}
	var matches []model.Session
	for _, item := range sessions {
		if strings.EqualFold(item.Target, target) || strings.EqualFold(item.Server, target) || strings.EqualFold(item.ServerID, target) {
			matches = append(matches, item)
		}
	}
	return matches
}

func launchDiagnosticStatus(result launchDiagnostic) (string, string) {
	if !result.DesktopAPI {
		return "failed", "desktop Opsera API is not reachable on 127.0.0.1:18741"
	}
	if strings.TrimSpace(result.Target) == "" {
		return "failed", "no bastion launch target was found in recent Opsera argv logs"
	}
	if !result.ServerPresent && !result.SessionPresent {
		return "failed", "target was parsed but is missing from both left server list and open sessions"
	}
	if !result.ServerPresent {
		return "failed", "target is in open sessions but missing from the left server list"
	}
	if !result.SessionPresent {
		return "failed", "target is in the left server list but no active session was opened"
	}
	return "ok", "bastion launch target is captured, listed, and active"
}

func runSSO(args []string) (bool, error) {
	if len(args) == 0 {
		return true, errors.New("usage: opsera sso attach|agent [--core-only]")
	}
	switch args[0] {
	case "status":
		result, handled, err := callSSOAgentNoStart("GET", "/sso/status", nil, 2*time.Second)
		if !handled {
			status := localSSOStatus(nil, "")
			if present, _ := status["xshellUrlPresent"].(bool); present {
				result, handled, err = callSSOAgent("GET", "/sso/status", nil, 10*time.Second)
				if handled {
					if err != nil {
						_ = writeJSON(result)
						return true, err
					}
					return true, writeJSON(result)
				}
			}
			status["agent"] = false
			if err != nil {
				status["agentError"] = err.Error()
			}
			return true, writeJSON(status)
		}
		if err != nil {
			_ = writeJSON(result)
			return true, err
		}
		return true, writeJSON(result)
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
		setSSOAgentCoreOnlyEnv(opts.CoreOnly)
		if opts.WindowKeepAlive {
			_ = os.Setenv("OPSERA_XSHELL_WINDOW_KEEPALIVE", "1")
		}
		if opts.NoBrowserKeepAlive {
			_ = os.Setenv("OPSERA_DASUSM_BROWSER_KEEPALIVE", "0")
		}
		result, handled, err := callSSOAgent("POST", "/sso/attach", map[string]bool{"coreOnly": opts.CoreOnly, "reset": true}, 45*time.Second)
		if !handled {
			status := localSSOStatus(nil, "")
			status["agent"] = false
			if err != nil {
				status["error"] = err.Error()
			}
			status["status"] = "failed"
			status["diagnosis"] = attachDiagnosis(status, err)
			_ = writeJSON(status)
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

func setSSOAgentCoreOnlyEnv(coreOnly bool) {
	if coreOnly {
		_ = os.Setenv("OPSERA_SSO_CORE_ONLY", "1")
		return
	}
	_ = os.Unsetenv("OPSERA_SSO_CORE_ONLY")
}

type ssoOptions struct {
	CoreOnly           bool
	WindowKeepAlive    bool
	NoBrowserKeepAlive bool
}

func parseSSOOptions(args []string) (ssoOptions, error) {
	opts := ssoOptions{}
	for _, arg := range args {
		switch arg {
		case "--core-only":
			opts.CoreOnly = true
		case "--window-keepalive":
			opts.WindowKeepAlive = true
		case "--no-browser-keepalive":
			opts.NoBrowserKeepAlive = true
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
		_ = logStore.Append(model.LogLevelWarn, "sso", "window keepalive is disabled because it can steal focus or flash the desktop; use Xshell's built-in keepalive instead", "")
	}
	if dasusmBrowserKeepAliveEnabled(opts) {
		go keepDASUSMBrowserAlive(logStore, dasusmBrowserKeepAliveInterval())
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ready", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		pool.Refresh()
		result := localSSOStatus(pool, "")
		result["ok"] = true
		_ = json.NewEncoder(w).Encode(result)
	})
	mux.HandleFunc("/sso/status", func(w http.ResponseWriter, _ *http.Request) {
		pool.Refresh()
		_ = json.NewEncoder(w).Encode(localSSOStatus(pool, ""))
	})
	mux.HandleFunc("/sso/attach", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			CoreOnly bool `json:"coreOnly"`
			Reset    bool `json:"reset"`
		}
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&req)
		}
		pool.SetCoreOnly(req.CoreOnly || envBool("OPSERA_SSO_CORE_ONLY"))
		if req.Reset {
			pool.ClearFailures()
		}
		err := pool.Attach()
		result := localSSOStatus(pool, "")
		result["status"] = statusFromErr(err)
		result["connected"] = err == nil
		if err != nil {
			result["diagnosis"] = attachDiagnosis(result, err)
		}
		writeSSOAgentResult(w, result, err)
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
		upload, err := pool.Upload(req.Local, req.Remote)
		result := upload.resultMap(req.Local, req.Remote)
		result["status"] = statusFromErr(err)
		writeSSOAgentResult(w, result, err)
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
		upload, err := pool.UploadLarge(req.Local, req.Remote, req.ChunkBytes)
		result := upload.resultMap(req.Local, req.Remote)
		result["status"] = statusFromErr(err)
		result["chunkBytes"] = req.ChunkBytes
		writeSSOAgentResult(w, result, err)
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
	return callSSOAgentHTTP(method, path, body, timeout)
}

func callSSOAgentNoStart(method, path string, body any, timeout time.Duration) (map[string]any, bool, error) {
	if !ssoAgentHealthy() {
		return nil, false, errors.New("sso agent is not running")
	}
	return callSSOAgentHTTP(method, path, body, timeout)
}

func callSSOAgentHTTP(method, path string, body any, timeout time.Duration) (map[string]any, bool, error) {
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
	hideCommandWindow(cmd)
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
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(ssoAgentBaseURL + "/ready")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func localSSOStatus(pool *DASUSMPool, lastError string) map[string]any {
	processes, _ := latestXshellProcessSnapshots()
	sessionsAll := sessionsFromXshellProcesses(processes, false)
	sessionsCore := sessionsFromXshellProcesses(processes, true)
	status := map[string]any{
		"agent":              pool != nil,
		"connected":          false,
		"opseraConnected":    false,
		"xshellUrlPresent":   len(sessionsAll) > 0,
		"xshellUrlCount":     len(sessionsAll),
		"xshellCoreUrlCount": len(sessionsCore),
		"xshellWindow":       xshellWindowVisible(),
	}
	if pool != nil {
		connected := pool.Connected()
		status["connected"] = connected
		status["opseraConnected"] = connected
		status["coreOnly"] = pool.CoreOnly()
	}
	if lastError != "" {
		status["lastAttachError"] = lastError
	}
	return status
}

func attachDiagnosis(status map[string]any, err error) string {
	if err == nil {
		return ""
	}
	xshellWindow, _ := status["xshellWindow"].(bool)
	xshellURL, _ := status["xshellUrlPresent"].(bool)
	if xshellWindow && xshellURL {
		return "Xshell window is online and an SSH URL is present, but Opsera could not authenticate with that URL. The URL token is likely expired or not reusable; reopen the SSH session from SSO and attach immediately."
	}
	if xshellWindow && !xshellURL {
		return "Xshell window is online, but no reusable SSH URL is visible to Opsera."
	}
	if !xshellWindow && xshellURL {
		return "An SSH URL is present, but no visible Xshell terminal window was found."
	}
	return "No visible Xshell terminal window and no reusable SSH URL were found."
}

func xshellWindowVisible() bool {
	found := false
	cb := syscall.NewCallback(func(hwnd uintptr, lparam uintptr) uintptr {
		if found || !windowVisible(hwnd) {
			return 1
		}
		title := windowTitle(hwnd)
		if strings.Contains(title, "GateShell - Xshell") || strings.Contains(title, " - Xshell") || title == "Xshell" {
			found = true
		}
		return 1
	})
	cliProcEnumWindows.Call(cb, 0)
	return found
}

func windowVisible(hwnd uintptr) bool {
	ok, _, _ := cliProcIsWindowVisible.Call(hwnd)
	return ok != 0
}

func windowTitle(hwnd uintptr) string {
	n, _, _ := cliProcGetWindowTextLength.Call(hwnd)
	if n == 0 {
		return ""
	}
	buf := make([]uint16, n+1)
	cliProcGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return syscall.UTF16ToString(buf)
}

func autoAttachDASUSM(pool *DASUSMPool, logStore *logs.Store) {
	connectedTicker := time.NewTicker(5 * time.Minute)
	defer connectedTicker.Stop()
	disconnectedTicker := time.NewTicker(15 * time.Second)
	defer disconnectedTicker.Stop()
	lastSignature := ""
	lastAttempt := time.Time{}
	lastErrorLog := time.Time{}
	for {
		interval := connectedTicker.C
		if !pool.Connected() {
			interval = disconnectedTicker.C
		}
		<-interval
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

func dasusmBrowserKeepAliveEnabled(opts ssoOptions) bool {
	if opts.NoBrowserKeepAlive {
		return false
	}
	value := strings.TrimSpace(os.Getenv("OPSERA_DASUSM_BROWSER_KEEPALIVE"))
	if value == "" {
		return true
	}
	return envBool("OPSERA_DASUSM_BROWSER_KEEPALIVE")
}

func dasusmBrowserKeepAliveInterval() time.Duration {
	value := strings.TrimSpace(os.Getenv("OPSERA_DASUSM_BROWSER_KEEPALIVE_INTERVAL_MINUTES"))
	if value == "" {
		return 18 * time.Minute
	}
	minutes, err := strconv.Atoi(value)
	if err != nil || minutes <= 0 {
		return 18 * time.Minute
	}
	if minutes < 3 {
		minutes = 3
	}
	return time.Duration(minutes) * time.Minute
}

func keepDASUSMBrowserAlive(logStore *logs.Store, interval time.Duration) {
	if interval <= 0 {
		interval = 18 * time.Minute
	}
	if logStore != nil {
		_ = logStore.Append(model.LogLevelInfo, "sso", fmt.Sprintf("DASUSM browser keepalive enabled: refresh every %s", interval), "")
	}
	refresh := func() {
		if err := refreshDASUSMBrowserWindow(); err != nil {
			if logStore != nil {
				_ = logStore.Append(model.LogLevelWarn, "sso", "DASUSM browser keepalive skipped: "+err.Error(), "")
			}
			return
		}
		if logStore != nil {
			_ = logStore.Append(model.LogLevelInfo, "sso", "DASUSM browser keepalive refreshed active page", "")
		}
	}
	refresh()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		refresh()
	}
}

func refreshDASUSMBrowserWindow() error {
	script := `$ErrorActionPreference = 'Stop'
Add-Type @"
using System;
using System.Text;
using System.Runtime.InteropServices;
public class OpseraBrowserKeepAlive {
  public delegate bool EnumWindowsProc(IntPtr hWnd, IntPtr lParam);
  [DllImport("user32.dll")] public static extern bool EnumWindows(EnumWindowsProc lpEnumFunc, IntPtr lParam);
  [DllImport("user32.dll")] public static extern bool IsWindowVisible(IntPtr hWnd);
  [DllImport("user32.dll")] public static extern int GetWindowText(IntPtr hWnd, StringBuilder text, int count);
  [DllImport("user32.dll")] public static extern uint GetWindowThreadProcessId(IntPtr hWnd, out uint processId);
  [DllImport("user32.dll")] public static extern IntPtr GetForegroundWindow();
  [DllImport("user32.dll")] public static extern bool SetForegroundWindow(IntPtr hWnd);
  [DllImport("user32.dll")] public static extern bool ShowWindowAsync(IntPtr hWnd, int nCmdShow);
  [DllImport("user32.dll")] public static extern bool IsWindow(IntPtr hWnd);
}
"@
$targets = New-Object System.Collections.Generic.List[object]
$callback = [OpseraBrowserKeepAlive+EnumWindowsProc]{
  param([IntPtr]$hWnd, [IntPtr]$lParam)
  if (-not [OpseraBrowserKeepAlive]::IsWindowVisible($hWnd)) { return $true }
  $builder = New-Object System.Text.StringBuilder 512
  [void][OpseraBrowserKeepAlive]::GetWindowText($hWnd, $builder, $builder.Capacity)
  $title = $builder.ToString()
  if ([string]::IsNullOrWhiteSpace($title)) { return $true }
  [uint32]$windowProcessId = 0
  [void][OpseraBrowserKeepAlive]::GetWindowThreadProcessId($hWnd, [ref]$windowProcessId)
  try { $process = Get-Process -Id $windowProcessId -ErrorAction Stop } catch { return $true }
  if ($process.ProcessName -notin @('chrome','msedge')) { return $true }
  $hostOpsTitle = -join ([char]0x4e3b,[char]0x673a,[char]0x8fd0,[char]0x7ef4)
  $firewallTitle = -join ([char]0x9632,[char]0x706b,[char]0x5899)
  $devCenterTitle = -join ([char]0x5f00,[char]0x53d1,[char]0x8005,[char]0x4e2d,[char]0x5fc3)
  $haiZhihuiTitle = -join ([char]0x6d77,[char]0x667a,[char]0x6c47)
  if ($title.Contains($hostOpsTitle) -or $title.Contains($firewallTitle) -or $title.Contains($devCenterTitle) -or $title.Contains($haiZhihuiTitle)) {
    $targets.Add([pscustomobject]@{ Hwnd = $hWnd; Title = $title; ProcessId = $windowProcessId }) | Out-Null
    return $true
  }
  if ($title -match '娑撶粯婧€鏉╂劗娣畖DASUSM|Plate|100\.200\.128\.13') {
    $targets.Add([pscustomobject]@{ Hwnd = $hWnd; Title = $title; ProcessId = $windowProcessId }) | Out-Null
  }
  return $true
}
[void][OpseraBrowserKeepAlive]::EnumWindows($callback, [IntPtr]::Zero)
if ($targets.Count -eq 0) { throw 'DASUSM browser window not found' }
$target = $targets[0]
$previous = [OpseraBrowserKeepAlive]::GetForegroundWindow()
$ws = New-Object -ComObject WScript.Shell
[void][OpseraBrowserKeepAlive]::ShowWindowAsync($target.Hwnd, 9)
Start-Sleep -Milliseconds 200
[void][OpseraBrowserKeepAlive]::SetForegroundWindow($target.Hwnd)
Start-Sleep -Milliseconds 300
$ws.SendKeys('{F5}')
Start-Sleep -Milliseconds 150
if ($previous -ne [IntPtr]::Zero -and $previous -ne $target.Hwnd -and [OpseraBrowserKeepAlive]::IsWindow($previous)) {
  [void][OpseraBrowserKeepAlive]::SetForegroundWindow($previous)
}
"refreshed $($target.Title)"`
	cmd := exec.Command("powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-Command", script)
	hideCommandWindow(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		text := strings.TrimSpace(string(out))
		if text == "" {
			text = err.Error()
		}
		return errors.New(text)
	}
	return nil
}

func hideCommandWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
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
	if strings.TrimSpace(xshPath) == "" {
		if session, ok := cachedLatestSession(); ok {
			out, err := runCommandWithSession(session, command)
			if err == nil {
				return out, nil
			}
			clearCachedLatestSession(session)
		}
	}
	session, err := LatestSession(xshPath)
	if err != nil {
		return "", err
	}
	out, err := runCommandWithSession(session, command)
	if err == nil {
		cacheLatestSession(session)
	}
	return out, err
}

func runCommandWithSession(session xsh.Session, command string) (string, error) {
	client, err := dialSession(session)
	if err != nil {
		return "", err
	}
	defer client.Close()
	return runWithClient(client, command)
}

func cachedLatestSession() (xsh.Session, bool) {
	latestSessionCacheMu.Lock()
	defer latestSessionCacheMu.Unlock()
	if latestSessionCache.Host == "" || latestSessionCache.Port <= 0 {
		return xsh.Session{}, false
	}
	return latestSessionCache, true
}

func cacheLatestSession(session xsh.Session) {
	if session.Host == "" || session.Port <= 0 {
		return
	}
	latestSessionCacheMu.Lock()
	latestSessionCache = session
	latestSessionCacheMu.Unlock()
}

func clearCachedLatestSession(session xsh.Session) {
	latestSessionCacheMu.Lock()
	defer latestSessionCacheMu.Unlock()
	if latestSessionCache.Host == session.Host && latestSessionCache.Port == session.Port {
		latestSessionCache = xsh.Session{}
	}
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
	return remoteRunInteractive(client, command)
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
		output, runErr := remoteRunInteractive(client, command)
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
	lastSeen    string
	lastRefresh time.Time
}

func NewDASUSMPool(logStore *logs.Store) *DASUSMPool {
	return &DASUSMPool{logs: logStore, failed: map[string]time.Time{}}
}

func (p *DASUSMPool) SetCoreOnly(coreOnly bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.coreOnly == coreOnly {
		return
	}
	p.coreOnly = coreOnly
	if p.client != nil {
		_ = p.client.Close()
		p.client = nil
	}
	p.failed = map[string]time.Time{}
}

func (p *DASUSMPool) CoreOnly() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.coreOnly
}

func (p *DASUSMPool) ClearFailures() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failed = map[string]time.Time{}
	p.lastRefresh = time.Time{}
}

func (p *DASUSMPool) Connected() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.client != nil && sshAlive(p.client)
}

func (p *DASUSMPool) Refresh() {
	processes, err := latestXshellProcessSnapshots()
	if err != nil {
		return
	}
	sessions := sessionsFromXshellProcesses(processes, p.CoreOnly())
	signature := sessionsSignature(sessions)
	if signature == "" {
		return
	}
	p.mu.Lock()
	if signature != p.lastSeen {
		p.failed = map[string]time.Time{}
		p.lastSeen = signature
		p.lastRefresh = time.Time{}
	}
	connected := p.client != nil && sshAlive(p.client)
	shouldAttach := !connected && time.Since(p.lastRefresh) > 5*time.Second
	if shouldAttach {
		p.lastRefresh = time.Now()
	}
	p.mu.Unlock()
	if shouldAttach {
		_ = p.Attach()
	}
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
	output, err := runInteractiveWithClient(client, command)
	if err == nil {
		return output, nil
	}
	p.drop()
	client, reconnectErr := p.clientForUse()
	if reconnectErr != nil {
		return output, fmt.Errorf("%w; reconnect failed: %v", err, reconnectErr)
	}
	return runInteractiveWithClient(client, command)
}

func (p *DASUSMPool) Upload(localPath, remotePath string) (uploadResult, error) {
	var result uploadResult
	err := p.withRetry(func(client *ssh.Client) error {
		var err error
		result, err = uploadWithClient(client, localPath, remotePath)
		return err
	})
	return result, err
}

func (p *DASUSMPool) Download(remotePath, localPath string) error {
	return p.withRetry(func(client *ssh.Client) error {
		return downloadWithClient(client, remotePath, localPath)
	})
}

func (p *DASUSMPool) UploadLarge(localPath, remotePath string, chunkSize int64) (uploadResult, error) {
	var result uploadResult
	err := p.withRetry(func(client *ssh.Client) error {
		var err error
		result, err = uploadLargeWithClient(client, localPath, remotePath, chunkSize)
		return err
	})
	return result, err
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
	if p.client != nil && sshAlive(p.client) {
		client := p.client
		p.mu.Unlock()
		return client, nil
	}
	if p.client != nil {
		_ = p.client.Close()
		p.client = nil
	}
	sessions, err := latestDASUSMSessions(p.coreOnly)
	if err != nil {
		p.mu.Unlock()
		return nil, err
	}
	signature := sessionsSignature(sessions)
	if signature != "" && signature != p.lastSeen {
		p.failed = map[string]time.Time{}
		p.lastSeen = signature
	}
	candidates := make([]xsh.Session, 0, len(sessions))
	for _, session := range sessions {
		if p.sessionBlocked(session) {
			continue
		}
		candidates = append(candidates, session)
	}
	p.mu.Unlock()

	failures := []string{}
	for _, session := range candidates {
		client, err := dialSession(session)
		if err != nil {
			p.mu.Lock()
			p.markSessionFailed(session)
			p.mu.Unlock()
			failures = append(failures, sessionFailure(session, err))
			continue
		}
		p.mu.Lock()
		if p.client != nil && sshAlive(p.client) {
			existing := p.client
			p.mu.Unlock()
			_ = client.Close()
			return existing, nil
		}
		p.client = client
		p.session = session
		p.connectedAt = time.Now()
		p.mu.Unlock()
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
	cmd := exec.Command("powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-Command", command)
	hideCommandWindow(cmd)
	out, err := cmd.Output()
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
	sum := sha256.Sum256([]byte(strings.Join(sessionPasswordVariants(session), "\x00")))
	return fmt.Sprintf("%s\x00%d\x00%s\x00%s", session.Host, session.Port, session.UserName, hex.EncodeToString(sum[:]))
}

func sessionsSignature(sessions []xsh.Session) string {
	if len(sessions) == 0 {
		return ""
	}
	keys := make([]string, 0, len(sessions))
	for _, session := range sessions {
		keys = append(keys, sessionCacheKey(session))
	}
	sort.Strings(keys)
	sum := sha256.Sum256([]byte(strings.Join(keys, "\n")))
	return hex.EncodeToString(sum[:])
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
	return fmt.Sprintf("%s@%s:%d (%d auth variant): %v", user, host, port, len(sessionPasswordVariants(session)), err)
}

func sessionPasswordVariants(session xsh.Session) []string {
	variants := []string{}
	add := func(value string) {
		if value == "" {
			return
		}
		for _, existing := range variants {
			if existing == value {
				return
			}
		}
		variants = append(variants, value)
	}
	add(session.Password)
	for _, value := range session.PasswordVariants {
		add(value)
	}
	return variants
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
	rawPassword := rawSSHURLPassword(raw)
	port := 22
	if parsed.Port() != "" {
		parsedPort, err := strconv.Atoi(parsed.Port())
		if err != nil || parsedPort <= 0 {
			return xsh.Session{}, errors.New("invalid ssh url port")
		}
		port = parsedPort
	}
	return xsh.Session{
		Host:             parsed.Hostname(),
		Port:             port,
		UserName:         parsed.User.Username(),
		Password:         password,
		PasswordVariants: []string{rawPassword},
	}, nil
}

func rawSSHURLPassword(raw string) string {
	withoutScheme := strings.TrimPrefix(raw, "ssh://")
	at := strings.LastIndex(withoutScheme, "@")
	if at < 0 {
		return ""
	}
	userInfo := withoutScheme[:at]
	colon := strings.Index(userInfo, ":")
	if colon < 0 {
		return ""
	}
	return userInfo[colon+1:]
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

func listOpenSessionsViaAPI() ([]model.Session, error) {
	client := http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://127.0.0.1:18741/sessions")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("opsera api returned %s", resp.Status)
	}
	var items []model.Session
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return nil, err
	}
	return items, nil
}

func listDesktopServersViaAPI() ([]model.Server, error) {
	client := http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://127.0.0.1:18741/servers")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("opsera api returned %s", resp.Status)
	}
	var items []model.Server
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return nil, err
	}
	return items, nil
}

func runOpenSessionCommandViaAPI(target string, command string) (model.Command, error) {
	body, err := json.Marshal(map[string]string{
		"target":  target,
		"command": command,
	})
	if err != nil {
		return model.Command{}, err
	}
	client := http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Post("http://127.0.0.1:18741/session/run", "application/json", bytes.NewReader(body))
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
	var lastErr error
	for _, password := range sessionPasswordVariants(session) {
		config := sshClientConfig(session.UserName, password)
		client, err := ssh.Dial("tcp", fmt.Sprintf("%s:%d", session.Host, session.Port), config)
		if err == nil {
			go keepAlive(client)
			return client, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no password variant available")
	}
	return nil, lastErr
}

func sshClientConfig(username, password string) *ssh.ClientConfig {
	return &ssh.ClientConfig{
		User: username,
		Auth: []ssh.AuthMethod{
			ssh.Password(password),
			ssh.KeyboardInteractive(func(user, instruction string, questions []string, echos []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range answers {
					answers[i] = password
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
		HostKeyAlgorithms: []string{
			ssh.KeyAlgoED25519,
			ssh.KeyAlgoECDSA256,
			ssh.KeyAlgoECDSA384,
			ssh.KeyAlgoECDSA521,
			ssh.KeyAlgoRSASHA256,
			ssh.KeyAlgoRSASHA512,
			ssh.KeyAlgoRSA,
			ssh.InsecureKeyAlgoDSA,
		},
	}
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
	var upload uploadResult
	var err error
	if strings.TrimSpace(serverRef) != "" {
		upload, err = uploadServerFile(serverRef, fileArgs[0], fileArgs[1])
	} else if useSSO {
		upload, err = uploadDASUSMFile(fileArgs[0], fileArgs[1])
	} else {
		upload, err = uploadFile(xshPath, fileArgs[0], fileArgs[1])
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
	result["size"] = upload.Size
	result["sha256"] = upload.SHA256
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
	serverRef := ""
	xshPath := ""
	useSSO := false
	chunkSize := int64(512 * 1024 * 1024)
	fileArgs := args
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
		return true, errors.New("usage: opsera file upload-large [--server <name|host|id>] [--xsh <path>] [--sso] [--chunk-mb 512] <local> <remote>")
	}
	result := map[string]any{"local": fileArgs[0], "remote": fileArgs[1], "chunkBytes": chunkSize}
	dataDir, _ := resolveDataDir()
	var upload uploadResult
	var err error
	if strings.TrimSpace(serverRef) != "" {
		upload, err = uploadLargeServerFile(serverRef, fileArgs[0], fileArgs[1], chunkSize)
	} else if useSSO {
		upload, err = uploadLargeDASUSMFile(fileArgs[0], fileArgs[1], chunkSize)
	} else {
		upload, err = uploadLargeFile(xshPath, fileArgs[0], fileArgs[1], chunkSize)
	}
	if err != nil {
		result["status"] = "failed"
		result["error"] = err.Error()
		_ = events.Write(dataDir, events.Event{Type: "upload-large", Command: "upload-large " + fileArgs[0] + " " + fileArgs[1], Error: err.Error(), Status: "failed"})
		_ = json.NewEncoder(os.Stdout).Encode(result)
		return true, err
	}
	result["status"] = "done"
	result["size"] = upload.Size
	result["sha256"] = upload.SHA256
	_ = events.Write(dataDir, events.Event{Type: "upload-large", Command: "upload-large " + fileArgs[0] + " " + fileArgs[1], Output: "uploaded", Status: "done"})
	return true, writeJSON(result)
}

type uploadResult struct {
	Size   int64
	SHA256 string
}

func (r uploadResult) resultMap(localPath, remotePath string) map[string]any {
	result := map[string]any{
		"local":  localPath,
		"remote": remotePath,
	}
	if r.Size > 0 || r.SHA256 != "" {
		result["size"] = r.Size
		result["sha256"] = r.SHA256
	}
	return result
}

func uploadFile(xshPath, localPath, remotePath string) (uploadResult, error) {
	session, err := LatestSession(xshPath)
	if err != nil {
		return uploadResult{}, err
	}
	return uploadSessionFile(session, localPath, remotePath)
}

func uploadServerFile(serverRef, localPath, remotePath string) (uploadResult, error) {
	session, err := serverSession(serverRef)
	if err != nil {
		return uploadResult{}, err
	}
	return uploadSessionFile(session, localPath, remotePath)
}

func uploadDASUSMFile(localPath, remotePath string) (uploadResult, error) {
	if result, handled, err := callSSOAgent("POST", "/sso/file/upload", map[string]string{
		"local":  localPath,
		"remote": remotePath,
	}, 10*time.Minute); handled {
		return uploadResultFromMap(result), err
	}
	sessions, err := LatestDASUSMSessions()
	if err != nil {
		return uploadResult{}, err
	}
	var upload uploadResult
	err = tryDASUSMSessions(sessions, func(session xsh.Session) error {
		var err error
		upload, err = uploadSessionFile(session, localPath, remotePath)
		return err
	})
	return upload, err
}

func uploadSessionFile(session xsh.Session, localPath, remotePath string) (uploadResult, error) {
	client, err := dialSession(session)
	if err != nil {
		return uploadResult{}, err
	}
	defer client.Close()
	return uploadWithClient(client, localPath, remotePath)
}

func uploadWithClient(client *ssh.Client, localPath, remotePath string) (uploadResult, error) {
	info, err := os.Stat(localPath)
	if err != nil {
		return uploadResult{}, err
	}
	if info.IsDir() {
		return uploadResult{}, fmt.Errorf("%s is a directory", localPath)
	}
	sha, err := fileSHA256(localPath)
	if err != nil {
		return uploadResult{}, err
	}
	result := uploadResult{Size: info.Size(), SHA256: sha}
	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		return uploadResult{}, err
	}
	defer sftpClient.Close()
	src, err := os.Open(localPath)
	if err != nil {
		return uploadResult{}, err
	}
	defer src.Close()
	if err := sftpClient.MkdirAll(path.Dir(remotePath)); err != nil {
		return uploadResult{}, err
	}
	tmpPath := remoteTempPath(remotePath)
	_ = sftpClient.Remove(tmpPath)
	dst, err := sftpClient.Create(tmpPath)
	if err != nil {
		return uploadResult{}, err
	}
	if _, err := dst.ReadFrom(src); err != nil {
		_ = dst.Close()
		_ = sftpClient.Remove(tmpPath)
		return uploadResult{}, err
	}
	if err := dst.Close(); err != nil {
		_ = sftpClient.Remove(tmpPath)
		return uploadResult{}, err
	}
	if err := verifyRemoteFile(client, sftpClient, tmpPath, result); err != nil {
		_ = sftpClient.Remove(tmpPath)
		return uploadResult{}, err
	}
	if err := promoteRemoteFile(client, tmpPath, remotePath); err != nil {
		_ = sftpClient.Remove(tmpPath)
		return uploadResult{}, err
	}
	if err := verifyRemoteFile(client, sftpClient, remotePath, result); err != nil {
		return uploadResult{}, err
	}
	return result, nil
}

func uploadLargeFile(xshPath, localPath, remotePath string, chunkSize int64) (uploadResult, error) {
	session, err := LatestSession(xshPath)
	if err != nil {
		return uploadResult{}, err
	}
	return uploadLargeSessionFile(session, localPath, remotePath, chunkSize)
}

func uploadLargeServerFile(serverRef, localPath, remotePath string, chunkSize int64) (uploadResult, error) {
	session, err := serverSession(serverRef)
	if err != nil {
		return uploadResult{}, err
	}
	return uploadLargeSessionFile(session, localPath, remotePath, chunkSize)
}

func uploadLargeDASUSMFile(localPath, remotePath string, chunkSize int64) (uploadResult, error) {
	if result, handled, err := callSSOAgent("POST", "/sso/file/upload-large", map[string]any{
		"local":      localPath,
		"remote":     remotePath,
		"chunkBytes": chunkSize,
	}, 24*time.Hour); handled {
		upload := uploadResultFromMap(result)
		if err != nil {
			return upload, err
		}
		if err := validateUploadLargeAgentResult(localPath, upload); err != nil {
			return uploadResult{}, err
		}
		return upload, nil
	}
	return uploadResult{}, errors.New("sso agent unavailable for upload-large; reopen SSO/Xshell and run `opsera sso attach` before retrying")
}

func validateUploadLargeAgentResult(localPath string, upload uploadResult) error {
	info, err := os.Stat(localPath)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory", localPath)
	}
	if upload.Size != info.Size() {
		return fmt.Errorf("sso upload-large returned incomplete result: local size %d remote result size %d", info.Size(), upload.Size)
	}
	if strings.TrimSpace(upload.SHA256) == "" {
		return errors.New("sso upload-large returned incomplete result: missing sha256")
	}
	localSHA, err := fileSHA256(localPath)
	if err != nil {
		return err
	}
	if upload.SHA256 != localSHA {
		return fmt.Errorf("sso upload-large returned sha256 mismatch: local %s remote result %s", localSHA, upload.SHA256)
	}
	return nil
}

func uploadLargeSessionFile(session xsh.Session, localPath, remotePath string, chunkSize int64) (uploadResult, error) {
	client, err := dialSession(session)
	if err != nil {
		return uploadResult{}, err
	}
	defer client.Close()
	return uploadLargeWithClient(client, localPath, remotePath, chunkSize)
}

func uploadLargeWithClient(client *ssh.Client, localPath, remotePath string, chunkSize int64) (uploadResult, error) {
	info, err := os.Stat(localPath)
	if err != nil {
		return uploadResult{}, err
	}
	if info.IsDir() {
		return uploadResult{}, fmt.Errorf("%s is a directory", localPath)
	}
	sha, err := fileSHA256(localPath)
	if err != nil {
		return uploadResult{}, err
	}
	result := uploadResult{Size: info.Size(), SHA256: sha}
	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		return uploadResult{}, err
	}
	defer sftpClient.Close()

	partsDir := remotePath + ".parts"
	if err := sftpClient.MkdirAll(partsDir); err != nil {
		return uploadResult{}, err
	}
	src, err := os.Open(localPath)
	if err != nil {
		return uploadResult{}, err
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
			return uploadResult{}, err
		}
		dst, err := sftpClient.Create(partPath + ".tmp")
		if err != nil {
			return uploadResult{}, err
		}
		copied, err := io.CopyBuffer(dst, io.LimitReader(src, partSize), buf)
		if err != nil {
			_ = dst.Close()
			_ = sftpClient.Remove(partPath + ".tmp")
			return uploadResult{}, err
		}
		if copied != partSize {
			_ = dst.Close()
			_ = sftpClient.Remove(partPath + ".tmp")
			return uploadResult{}, fmt.Errorf("local chunk short read %s: expected %d copied %d", partPath, partSize, copied)
		}
		if err := dst.Close(); err != nil {
			_ = sftpClient.Remove(partPath + ".tmp")
			return uploadResult{}, err
		}
		tmpStat, err := sftpClient.Stat(partPath + ".tmp")
		if err != nil {
			_ = sftpClient.Remove(partPath + ".tmp")
			return uploadResult{}, fmt.Errorf("remote part tmp missing %s.tmp: %w", partPath, err)
		}
		if tmpStat.Size() != partSize {
			_ = sftpClient.Remove(partPath + ".tmp")
			return uploadResult{}, fmt.Errorf("remote part tmp size mismatch %s.tmp: expected %d remote %d", partPath, partSize, tmpStat.Size())
		}
		if err := sftpClient.Rename(partPath+".tmp", partPath); err != nil {
			_ = sftpClient.Remove(partPath + ".tmp")
			return uploadResult{}, err
		}
		partStat, err := sftpClient.Stat(partPath)
		if err != nil {
			return uploadResult{}, fmt.Errorf("remote part missing after rename %s: %w", partPath, err)
		}
		if partStat.Size() != partSize {
			return uploadResult{}, fmt.Errorf("remote part size mismatch after rename %s: expected %d remote %d", partPath, partSize, partStat.Size())
		}
	}
	for i := 0; i < chunks; i++ {
		partSize := chunkSize
		if remain := total - int64(i)*chunkSize; remain < partSize {
			partSize = remain
		}
		partPath := fmt.Sprintf("%s/part-%06d", partsDir, i)
		st, err := sftpClient.Stat(partPath)
		if err != nil {
			return uploadResult{}, fmt.Errorf("remote part missing %s: %w", partPath, err)
		}
		if st.Size() != partSize {
			return uploadResult{}, fmt.Errorf("remote part size mismatch %s: local %d remote %d", partPath, partSize, st.Size())
		}
	}
	tmpPath := remoteTempPath(remotePath)
	_ = sftpClient.Remove(tmpPath)
	merge := fmt.Sprintf("cat %s/part-* > %s && sha256sum %s", shellQuote(partsDir), shellQuote(tmpPath), shellQuote(tmpPath))
	out, err := remoteRun(client, merge)
	if err != nil {
		_ = sftpClient.Remove(tmpPath)
		return uploadResult{}, fmt.Errorf("%w: %s", err, out)
	}
	if !strings.HasPrefix(strings.TrimSpace(out), sha) {
		_ = sftpClient.Remove(tmpPath)
		return uploadResult{}, fmt.Errorf("sha256 mismatch: local %s remote %s", sha, strings.TrimSpace(out))
	}
	if err := verifyRemoteFile(client, sftpClient, tmpPath, result); err != nil {
		_ = sftpClient.Remove(tmpPath)
		return uploadResult{}, err
	}
	if err := promoteRemoteFile(client, tmpPath, remotePath); err != nil {
		_ = sftpClient.Remove(tmpPath)
		return uploadResult{}, err
	}
	if err := verifyRemoteFile(client, sftpClient, remotePath, result); err != nil {
		return uploadResult{}, err
	}
	_, _ = remoteRun(client, "rm -rf -- "+shellQuote(partsDir))
	return result, nil
}

func uploadResultFromMap(result map[string]any) uploadResult {
	if result == nil {
		return uploadResult{}
	}
	upload := uploadResult{}
	switch size := result["size"].(type) {
	case float64:
		upload.Size = int64(size)
	case int64:
		upload.Size = size
	case int:
		upload.Size = int64(size)
	case json.Number:
		if value, err := size.Int64(); err == nil {
			upload.Size = value
		}
	}
	if sha, _ := result["sha256"].(string); sha != "" {
		upload.SHA256 = sha
	}
	return upload
}

func remoteTempPath(remotePath string) string {
	return remotePath + ".new"
}

func verifyRemoteFile(client *ssh.Client, sftpClient *sftp.Client, remotePath string, expected uploadResult) error {
	st, err := sftpClient.Stat(remotePath)
	if err != nil {
		return fmt.Errorf("remote file missing %s: %w", remotePath, err)
	}
	if st.Size() != expected.Size {
		return fmt.Errorf("remote size mismatch %s: local %d remote %d", remotePath, expected.Size, st.Size())
	}
	out, err := remoteRun(client, "sha256sum "+shellQuote(remotePath))
	if err != nil {
		return fmt.Errorf("remote sha256 failed for %s: %w: %s", remotePath, err, out)
	}
	remoteSHA := strings.Fields(strings.TrimSpace(out))
	if len(remoteSHA) == 0 || remoteSHA[0] != expected.SHA256 {
		return fmt.Errorf("remote sha256 mismatch %s: local %s remote %s", remotePath, expected.SHA256, strings.TrimSpace(out))
	}
	return nil
}

func promoteRemoteFile(client *ssh.Client, tmpPath, remotePath string) error {
	out, err := remoteRun(client, "mv -f -- "+shellQuote(tmpPath)+" "+shellQuote(remotePath))
	if err != nil {
		return fmt.Errorf("remote promote failed: %w: %s", err, out)
	}
	return nil
}

func remoteRun(client *ssh.Client, command string) (string, error) {
	return runWithClient(client, command)
}

func remoteRunInteractive(client *ssh.Client, command string) (string, error) {
	return runInteractiveWithClient(client, command)
}

func interactiveShellCommand(command string) string {
	return "bash -lic " + strconv.Quote(command)
}

func runInteractiveWithClient(client *ssh.Client, command string) (string, error) {
	session, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()
	out, err := runInteractiveSession(session, command)
	return string(out), err
}

func runInteractiveSession(session *ssh.Session, command string) ([]byte, error) {
	_ = session.RequestPty("xterm-256color", 40, 180, ssh.TerminalModes{
		ssh.ECHO:          0,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	})
	return session.CombinedOutput(interactiveShellCommand(command))
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
	var fallbackFailures []string
	for _, path := range candidates {
		session, err := xsh.Parse(path)
		if err != nil {
			continue
		}
		if first.Path == "" {
			first = session
		}
		if tcpAlive(session.Host, session.Port) {
			if !isLocalHost(session.Host) {
				return session, nil
			}
			err := probeTunnelSession(session)
			if err == nil {
				return session, nil
			}
			if len(fallbackFailures) == 0 {
				fallbackFailures = append(fallbackFailures, fmt.Sprintf("%s:%d %v", session.Host, session.Port, err))
			}
		}
		if candidate, failures, ok := liveTunnelSessionCandidate(session, localTunnelListenPorts()); ok {
			return candidate, nil
		} else if len(failures) > 0 && len(fallbackFailures) == 0 {
			fallbackFailures = failures
		}
	}
	if first.Path != "" {
		if len(fallbackFailures) > 0 {
			return xsh.Session{}, fmt.Errorf("no active ssh tunnel found; last session is %s:%d from %s; scanned tunnel ports: %s", first.Host, first.Port, first.Path, strings.Join(fallbackFailures, "; "))
		}
		return xsh.Session{}, fmt.Errorf("no active ssh tunnel found; last session is %s:%d from %s", first.Host, first.Port, first.Path)
	}
	return xsh.Session{}, errors.New("no readable xshell session file found")
}

func liveTunnelSessionCandidate(base xsh.Session, ports []int) (xsh.Session, []string, bool) {
	if !isLocalHost(base.Host) || len(ports) == 0 {
		return xsh.Session{}, nil, false
	}
	failures := []string{}
	for _, port := range ports {
		if port <= 0 || port == base.Port {
			continue
		}
		candidate := base
		candidate.Host = "127.0.0.1"
		candidate.Port = port
		err := probeTunnelSession(candidate)
		if err == nil {
			return candidate, failures, true
		}
		failures = append(failures, fmt.Sprintf("127.0.0.1:%d %v", port, err))
	}
	return xsh.Session{}, failures, false
}

func defaultProbeTunnelSession(session xsh.Session) error {
	var lastErr error
	for _, password := range sessionPasswordVariants(session) {
		config := sshClientConfig(session.UserName, password)
		config.Timeout = 1500 * time.Millisecond
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", session.Host, session.Port), 1500*time.Millisecond)
		if err != nil {
			lastErr = err
			continue
		}
		_ = conn.SetDeadline(time.Now().Add(1500 * time.Millisecond))
		sshConn, chans, reqs, err := ssh.NewClientConn(conn, fmt.Sprintf("%s:%d", session.Host, session.Port), config)
		if err != nil {
			_ = conn.Close()
			lastErr = err
			continue
		}
		client := ssh.NewClient(sshConn, chans, reqs)
		return client.Close()
	}
	if lastErr == nil {
		lastErr = errors.New("no password variant available")
	}
	return lastErr
}

func isLocalHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

func localTunnelListenPorts() []int {
	cmd := exec.Command("powershell.exe", "-NoLogo", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", `
$ErrorActionPreference='SilentlyContinue'
Get-NetTCPConnection -State Listen |
  Where-Object { $_.LocalAddress -eq '127.0.0.1' } |
  ForEach-Object {
    $p = Get-Process -Id $_.OwningProcess -ErrorAction SilentlyContinue
    if ($p -and $p.ProcessName -match '^(tunnel|putty|Xshell|XshellCore)$') {
      [string]$_.LocalPort
    }
  }
`)
	hideCommandWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	seen := map[int]bool{}
	ports := []int{}
	for _, line := range strings.Fields(string(out)) {
		port, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil || port <= 0 || seen[port] {
			continue
		}
		seen[port] = true
		ports = append(ports, port)
	}
	sort.Ints(ports)
	return ports
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
