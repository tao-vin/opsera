package app

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tao-vin/opsera/internal/logs"
	"github.com/tao-vin/opsera/internal/model"
	"github.com/tao-vin/opsera/internal/relayws"
)

func RunRelayAgentForCLI(relayHost, machineCode, password, secret string) error {
	if strings.TrimSpace(relayHost) == "" || strings.TrimSpace(machineCode) == "" || strings.TrimSpace(password) == "" || strings.TrimSpace(secret) == "" {
		return fmt.Errorf("host, code, password and secret are required")
	}
	dataDir, err := resolveDataDir()
	if err != nil {
		return err
	}
	logStore, err := logs.NewStore(filepath.Join(dataDir, "logs"))
	if err != nil {
		return err
	}
	r := &runtime{
		logStore:  logStore,
		dataDir:   dataDir,
		relayHost: relayHost,
		identity: localIdentity{
			MachineCode: machineCode,
			Password:    password,
			Secret:      secret,
		},
	}
	if err := r.registerRelayDevice(); err != nil {
		return err
	}
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			_ = r.heartbeatRelayDevice()
		}
	}()
	for {
		if err := r.runRelayAgent(); err != nil {
			time.Sleep(3 * time.Second)
		}
	}
}

func (r *runtime) startRelayAgent() {
	if strings.TrimSpace(r.relayHost) == "" || strings.TrimSpace(r.identity.Secret) == "" {
		return
	}
	go func() {
		for {
			if err := r.runRelayAgent(); err != nil {
				_ = r.logStore.Append(model.LogLevelWarn, "relay", "agent disconnected: "+err.Error(), "")
			}
			time.Sleep(3 * time.Second)
		}
	}()
}

func (r *runtime) runRelayAgent() error {
	u := relayWSURL(r.relayHost, "/agent/ws")
	u += "?machineCode=" + r.identity.MachineCode + "&secret=" + r.identity.Secret
	conn, err := relayws.Dial(u)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = r.logStore.Append(model.LogLevelInfo, "relay", "agent connected", "")
	var writeMu sync.Mutex
	for {
		var msg relayws.Message
		if err := conn.ReadJSON(&msg); err != nil {
			return err
		}
		go func(msg relayws.Message) {
			result := r.handleRelayTask(msg)
			writeMu.Lock()
			defer writeMu.Unlock()
			_ = conn.WriteJSON(result)
		}(msg)
	}
}

func (r *runtime) handleRelayTask(msg relayws.Message) relayws.Message {
	result := relayws.Message{Type: "result", ID: msg.ID}
	switch msg.Type {
	case "ping":
		result.Output = "pong"
	case "exec":
		output, exitCode, err := runPowerShellTask(msg.Command)
		result.Output = output
		result.ExitCode = exitCode
		if err != nil {
			result.Error = err.Error()
		}
	case "upload":
		err := writeRelayUpload(msg.Path, msg.Data)
		if err != nil {
			result.Error = err.Error()
		} else {
			result.Output = "uploaded " + msg.Path
		}
	case "upload-append":
		err := appendRelayUpload(msg.Path, msg.Data)
		if err != nil {
			result.Error = err.Error()
		} else {
			result.Output = "appended " + msg.Path
		}
	case "download-file":
		err := downloadRelayFile(msg)
		if err != nil {
			result.Error = err.Error()
		} else {
			result.Output = "downloaded " + msg.Path
		}
	case "file-upload":
		_ = r.logStore.Append(model.LogLevelInfo, "relay", "file-upload start: "+msg.ID+" -> "+msg.Path, msg.ID)
		err := r.receiveRelayFile(msg)
		if err != nil {
			_ = r.logStore.Append(model.LogLevelError, "relay", "file-upload failed: "+err.Error(), msg.ID)
			result.Error = err.Error()
		} else {
			_ = r.logStore.Append(model.LogLevelInfo, "relay", "file-upload done: "+msg.Path, msg.ID)
			result.Output = fmt.Sprintf("uploaded %s size=%d sha256=%s", msg.Path, msg.Size, msg.SHA256)
		}
	case "restart":
		exe := strings.TrimSpace(msg.Executable)
		if exe == "" {
			exe, _ = os.Executable()
		}
		if err := startDetached(exe); err != nil {
			result.Error = err.Error()
		} else {
			result.Output = "restarting"
			go func() {
				time.Sleep(500 * time.Millisecond)
				os.Exit(0)
			}()
		}
	default:
		result.Error = "unknown relay task: " + msg.Type
	}
	return result
}

type relayFileAck struct {
	Type  string `json:"type"`
	Index int64  `json:"index,omitempty"`
	Error string `json:"error,omitempty"`
}

func (r *runtime) receiveRelayFile(msg relayws.Message) error {
	target := strings.TrimSpace(msg.Path)
	if target == "" {
		return fmt.Errorf("path is required")
	}
	if strings.TrimSpace(msg.ID) == "" {
		return fmt.Errorf("task id is required")
	}
	if msg.Size < 0 || strings.TrimSpace(msg.SHA256) == "" {
		return fmt.Errorf("file size and sha256 are required")
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	u := relayWSURL(r.relayHost, "/agent/file/ws")
	values := url.Values{}
	values.Set("machineCode", r.identity.MachineCode)
	values.Set("secret", r.identity.Secret)
	values.Set("taskId", msg.ID)
	u += "?" + values.Encode()
	conn, err := relayws.Dial(u)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = r.logStore.Append(model.LogLevelInfo, "relay", "file-upload channel ready: "+msg.ID, msg.ID)
	if err := conn.WriteJSON(relayFileAck{Type: "ready"}); err != nil {
		return err
	}
	tmp := target + ".opsera-new"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	hash := sha256.New()
	var total int64
	var expectedIndex int64
	cleanup := true
	defer func() {
		_ = out.Close()
		if cleanup {
			_ = os.Remove(tmp)
		}
	}()
	for {
		opcode, payload, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		if opcode == 1 {
			var frame relayFileAck
			if err := json.Unmarshal(payload, &frame); err != nil {
				return err
			}
			if frame.Type == "done" {
				break
			}
			continue
		}
		if opcode != 2 {
			continue
		}
		if len(payload) < 8 {
			return fmt.Errorf("invalid file frame")
		}
		index := int64(binary.BigEndian.Uint64(payload[:8]))
		if index != expectedIndex {
			_ = conn.WriteJSON(relayFileAck{Type: "error", Index: expectedIndex, Error: fmt.Sprintf("unexpected chunk %d", index)})
			return fmt.Errorf("unexpected chunk %d want %d", index, expectedIndex)
		}
		data := payload[8:]
		n, err := out.Write(data)
		if err != nil {
			_ = conn.WriteJSON(relayFileAck{Type: "error", Index: index, Error: err.Error()})
			return err
		}
		if n != len(data) {
			err := io.ErrShortWrite
			_ = conn.WriteJSON(relayFileAck{Type: "error", Index: index, Error: err.Error()})
			return err
		}
		if _, err := hash.Write(data); err != nil {
			_ = conn.WriteJSON(relayFileAck{Type: "error", Index: index, Error: err.Error()})
			return err
		}
		total += int64(len(data))
		if index == 0 || index%32 == 0 {
			_ = r.logStore.Append(model.LogLevelInfo, "relay", fmt.Sprintf("file-upload chunk index=%d total=%d", index, total), msg.ID)
		}
		if err := conn.WriteJSON(relayFileAck{Type: "ack", Index: index}); err != nil {
			return err
		}
		expectedIndex++
	}
	if err := out.Close(); err != nil {
		return err
	}
	if total != msg.Size {
		return fmt.Errorf("size mismatch: got %d want %d", total, msg.Size)
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if !strings.EqualFold(actual, msg.SHA256) {
		return fmt.Errorf("sha256 mismatch: got %s want %s", actual, msg.SHA256)
	}
	if err := os.Rename(tmp, target); err != nil {
		return err
	}
	cleanup = false
	return conn.WriteJSON(relayFileAck{Type: "complete"})
}

func runPowerShellTask(command string) (string, int, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return "", 1, fmt.Errorf("command is required")
	}
	timeout := relayExecTimeout()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoLogo", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", command)
	hideCommandWindow(cmd)
	out, err := cmd.CombinedOutput()
	exitCode := 0
	if ctx.Err() == context.DeadlineExceeded {
		return string(out), 124, fmt.Errorf("relay exec timed out after %s", timeout)
	}
	if err != nil {
		exitCode = 1
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
	}
	return string(out), exitCode, err
}

func relayExecTimeout() time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(os.Getenv("OPSERA_RELAY_EXEC_TIMEOUT_SECONDS")))
	if err != nil || seconds <= 0 {
		seconds = 45
	}
	if seconds < 5 {
		seconds = 5
	}
	if seconds > 600 {
		seconds = 600
	}
	return time.Duration(seconds) * time.Second
}

func writeRelayUpload(pathValue string, encoded string) error {
	pathValue = strings.TrimSpace(pathValue)
	if pathValue == "" {
		return fmt.Errorf("path is required")
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(pathValue), 0o755); err != nil {
		return err
	}
	tmp := pathValue + ".opsera-new"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, pathValue)
}

func appendRelayUpload(pathValue string, encoded string) error {
	pathValue = strings.TrimSpace(pathValue)
	if pathValue == "" {
		return fmt.Errorf("path is required")
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(pathValue), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(pathValue, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o755)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}

func downloadRelayFile(msg relayws.Message) error {
	target := strings.TrimSpace(msg.Path)
	if target == "" {
		return fmt.Errorf("path is required")
	}
	if msg.FileID == "" || msg.PartCount <= 0 || msg.PartSize <= 0 || msg.SHA256 == "" {
		return fmt.Errorf("file metadata is incomplete")
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	tmp := target + ".opsera-new"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	hash := sha256.New()
	w := io.MultiWriter(out, hash)
	client := http.Client{Timeout: 30 * time.Second}
	for i := 0; i < msg.PartCount; i++ {
		if err := downloadRelayPart(&client, msg, i, w); err != nil {
			_ = out.Close()
			_ = os.Remove(tmp)
			return err
		}
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if !strings.EqualFold(actual, msg.SHA256) {
		_ = os.Remove(tmp)
		return fmt.Errorf("sha256 mismatch: local %s remote %s", actual, msg.SHA256)
	}
	if msg.Size >= 0 {
		info, err := os.Stat(tmp)
		if err != nil {
			_ = os.Remove(tmp)
			return err
		}
		if info.Size() != msg.Size {
			_ = os.Remove(tmp)
			return fmt.Errorf("size mismatch: got %d want %d", info.Size(), msg.Size)
		}
	}
	return os.Rename(tmp, target)
}

func downloadRelayPart(client *http.Client, msg relayws.Message, index int, out io.Writer) error {
	base := strings.TrimRight(msg.RelayURL, "/")
	if base == "" {
		return fmt.Errorf("relay url is required")
	}
	partURL := base + "/files/" + url.PathEscape(msg.FileID) + "/parts/" + strconv.Itoa(index)
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		resp, err := client.Get(partURL)
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("download part %d failed: %s: %s", index, resp.Status, strings.TrimSpace(string(data)))
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}
		_, err = io.Copy(out, resp.Body)
		closeErr := resp.Body.Close()
		if err == nil {
			err = closeErr
		}
		if err == nil {
			return nil
		}
		lastErr = err
		time.Sleep(time.Duration(attempt+1) * time.Second)
	}
	return lastErr
}

func relayWSURL(host, path string) string {
	base := relayURL(host, path)
	if strings.HasPrefix(base, "https://") {
		return "wss://" + strings.TrimPrefix(base, "https://")
	}
	if strings.HasPrefix(base, "http://") {
		return "ws://" + strings.TrimPrefix(base, "http://")
	}
	return base
}
