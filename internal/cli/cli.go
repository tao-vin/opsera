package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/tao-vin/opsera/internal/events"
	"github.com/tao-vin/opsera/internal/logs"
	"github.com/tao-vin/opsera/internal/model"
	"github.com/tao-vin/opsera/internal/xsh"
)

var xshPathPattern = regexp.MustCompile(`[A-Za-z]:\\[^"]+\.xsh`)

func TryRun(args []string) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	if args[0] == "file" {
		return runFile(args[1:])
	}
	if args[0] != "command" {
		return false, nil
	}
	if len(args) < 3 || args[1] != "run" {
		return true, errors.New("usage: opsera command run [--xsh <path>] <shell-command>")
	}
	xshPath := ""
	commandArgs := args[2:]
	if len(commandArgs) >= 2 && commandArgs[0] == "--xsh" {
		xshPath = commandArgs[1]
		commandArgs = commandArgs[2:]
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
	output, runErr := RunCommand(xshPath, command)
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
	return true, json.NewEncoder(os.Stdout).Encode(item)
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

func OpenKeepAlive(xshPath string) (*ssh.Client, error) {
	session, err := LatestSession(xshPath)
	if err != nil {
		return nil, err
	}
	return dialSession(session)
}

func dialSession(session xsh.Session) (*ssh.Client, error) {
	config := &ssh.ClientConfig{
		User:            session.UserName,
		Auth:            []ssh.AuthMethod{ssh.Password(session.Password)},
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
		return true, errors.New("usage: opsera file upload|upload-large|download [--xsh <path>] <local> <remote>")
	}
	if args[0] == "upload-large" {
		return runUploadLarge(args[1:])
	}
	if args[0] == "download" {
		return runDownload(args[1:])
	}
	if args[0] != "upload" {
		return true, errors.New("usage: opsera file upload [--xsh <path>] <local> <remote>")
	}
	xshPath := ""
	fileArgs := args[1:]
	if len(fileArgs) >= 2 && fileArgs[0] == "--xsh" {
		xshPath = fileArgs[1]
		fileArgs = fileArgs[2:]
	}
	if len(fileArgs) != 2 {
		return true, errors.New("usage: opsera file upload [--xsh <path>] <local> <remote>")
	}
	result := map[string]any{
		"local":  fileArgs[0],
		"remote": fileArgs[1],
	}
	dataDir, _ := resolveDataDir()
	err := uploadFile(xshPath, fileArgs[0], fileArgs[1])
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
	return true, json.NewEncoder(os.Stdout).Encode(result)
}

func runDownload(args []string) (bool, error) {
	xshPath := ""
	fileArgs := args
	if len(fileArgs) >= 2 && fileArgs[0] == "--xsh" {
		xshPath = fileArgs[1]
		fileArgs = fileArgs[2:]
	}
	if len(fileArgs) != 2 {
		return true, errors.New("usage: opsera file download [--xsh <path>] <remote> <local>")
	}
	result := map[string]any{
		"remote": fileArgs[0],
		"local":  fileArgs[1],
	}
	dataDir, _ := resolveDataDir()
	err := downloadFile(xshPath, fileArgs[0], fileArgs[1])
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
	return true, json.NewEncoder(os.Stdout).Encode(result)
}

func runUploadLarge(args []string) (bool, error) {
	xshPath := ""
	chunkSize := int64(512 * 1024 * 1024)
	fileArgs := args
	for len(fileArgs) >= 2 {
		if fileArgs[0] == "--xsh" {
			xshPath = fileArgs[1]
			fileArgs = fileArgs[2:]
			continue
		}
		if fileArgs[0] == "--chunk-mb" {
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
		return true, errors.New("usage: opsera file upload-large [--xsh <path>] [--chunk-mb 512] <local> <remote>")
	}
	result := map[string]any{"local": fileArgs[0], "remote": fileArgs[1], "chunkBytes": chunkSize}
	dataDir, _ := resolveDataDir()
	err := uploadLargeFile(xshPath, fileArgs[0], fileArgs[1], chunkSize)
	if err != nil {
		result["status"] = "failed"
		result["error"] = err.Error()
		_ = events.Write(dataDir, events.Event{Type: "upload-large", Command: "upload-large " + fileArgs[0] + " " + fileArgs[1], Error: err.Error(), Status: "failed"})
		_ = json.NewEncoder(os.Stdout).Encode(result)
		return true, err
	}
	result["status"] = "done"
	_ = events.Write(dataDir, events.Event{Type: "upload-large", Command: "upload-large " + fileArgs[0] + " " + fileArgs[1], Output: "uploaded", Status: "done"})
	return true, json.NewEncoder(os.Stdout).Encode(result)
}

func uploadFile(xshPath, localPath, remotePath string) error {
	session, err := LatestSession(xshPath)
	if err != nil {
		return err
	}
	client, err := dialSession(session)
	if err != nil {
		return err
	}
	defer client.Close()
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
	info, err := os.Stat(localPath)
	if err != nil {
		return err
	}
	sha, err := fileSHA256(localPath)
	if err != nil {
		return err
	}
	session, err := LatestSession(xshPath)
	if err != nil {
		return err
	}
	client, err := dialSession(session)
	if err != nil {
		return err
	}
	defer client.Close()
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
	client, err := dialSession(session)
	if err != nil {
		return err
	}
	defer client.Close()
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
