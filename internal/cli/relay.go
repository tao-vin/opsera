package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tao-vin/opsera/internal/relayws"
)

const defaultRelayHost = "opsera.deepcode.art"

const (
	relayMultipartThreshold int64 = 1 << 20
	relayMultipartPartSize  int64 = 128 << 10
)

func runRelay(args []string) (bool, error) {
	if len(args) == 0 {
		return true, errors.New("usage: opsera relay status|exec|upload|deploy|agent ...")
	}
	switch args[0] {
	case "agent":
		return runRelayAgentCLI(args[1:])
	case "status":
		if len(args) < 3 {
			return true, errors.New("usage: opsera relay status <code> <password> [relay-host]")
		}
		host := relayArgHost(args, 3)
		device, err := relayVerify(host, args[1], args[2])
		if err != nil {
			return true, err
		}
		return true, writeJSON(device)
	case "exec":
		if len(args) < 4 {
			return true, errors.New("usage: opsera relay exec <code> <password> <powershell-command> [relay-host]")
		}
		host := defaultRelayHost
		commandArgs := args[3:]
		if len(commandArgs) > 1 && isRelayHostArg(commandArgs[len(commandArgs)-1]) {
			host = commandArgs[len(commandArgs)-1]
			commandArgs = commandArgs[:len(commandArgs)-1]
		}
		result, err := relayRequest(host, args[1], args[2], relayws.Message{
			Type:    "exec",
			ID:      newRelayTaskID(),
			Command: strings.Join(commandArgs, " "),
		})
		if err != nil {
			return true, err
		}
		return true, writeJSON(result)
	case "upload":
		if len(args) < 5 || len(args) > 6 {
			return true, errors.New("usage: opsera relay upload <code> <password> <local> <remote> [relay-host]")
		}
		host := relayArgHost(args, 5)
		return true, relayUploadChunked(host, args[1], args[2], args[3], args[4], false)
	case "deploy":
		if len(args) < 4 || len(args) > 6 {
			return true, errors.New("usage: opsera relay deploy <code> <password> <local-exe> [remote-exe] [relay-host]")
		}
		remote := `D:\application\opsera\opsera.exe`
		if len(args) == 5 {
			remote = args[4]
		}
		host := defaultRelayHost
		if len(args) == 6 {
			remote = args[4]
			host = args[5]
		}
		if err := relayDeploy(host, args[1], args[2], args[3], remote); err != nil {
			return true, err
		}
		return true, writeJSON(map[string]any{"ok": true, "remote": remote})
	default:
		return true, errors.New("usage: opsera relay status|exec|upload|deploy|agent ...")
	}
}

var runRelayAgentHook func(host, code, password, secret string) error

func SetRelayAgentRunner(fn func(host, code, password, secret string) error) {
	runRelayAgentHook = fn
}

func runRelayAgentCLI(args []string) (bool, error) {
	host := defaultRelayHost
	code := ""
	password := ""
	secret := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--host":
			i++
			if i >= len(args) {
				return true, errors.New("--host requires a value")
			}
			host = args[i]
		case "--code":
			i++
			if i >= len(args) {
				return true, errors.New("--code requires a value")
			}
			code = args[i]
		case "--password":
			i++
			if i >= len(args) {
				return true, errors.New("--password requires a value")
			}
			password = args[i]
		case "--secret":
			i++
			if i >= len(args) {
				return true, errors.New("--secret requires a value")
			}
			secret = args[i]
		default:
			return true, fmt.Errorf("unknown relay agent option: %s", args[i])
		}
	}
	if runRelayAgentHook == nil {
		return true, errors.New("relay agent is unavailable in this build")
	}
	return true, runRelayAgentHook(host, code, password, secret)
}

func relayDeploy(host, code, password, localExe, remoteExe string) error {
	manifest, err := uploadFileToRelay(host, localExe)
	if err != nil {
		return err
	}
	cmd := fmt.Sprintf(`$ErrorActionPreference='Stop'; $target=%s; $dir=Split-Path -Parent $target; New-Item -ItemType Directory -Force -Path $dir | Out-Null; $script=Join-Path $env:TEMP ('opsera-deploy-' + [guid]::NewGuid().ToString() + '.ps1'); @'
$ErrorActionPreference='Stop'
$target=%s
$relay=%s
$fileId=%s
$partCount=%d
$expectedSize=%d
$expectedHash=%s
$dir=Split-Path -Parent $target
$tmp=$target + '.opsera-new'
New-Item -ItemType Directory -Force -Path $dir | Out-Null
Remove-Item -LiteralPath $tmp -Force -ErrorAction SilentlyContinue
$out=[IO.File]::Open($tmp,[IO.FileMode]::Create,[IO.FileAccess]::Write,[IO.FileShare]::None)
try {
  for ($i=0; $i -lt $partCount; $i++) {
    $url=$relay.TrimEnd('/') + '/files/' + $fileId + '/parts/' + $i
    $client=[Net.Http.HttpClient]::new()
    try {
      $bytes=$client.GetByteArrayAsync($url).GetAwaiter().GetResult()
    } finally {
      $client.Dispose()
    }
    $out.Write($bytes,0,$bytes.Length)
  }
} finally {
  $out.Close()
}
$item=Get-Item -LiteralPath $tmp
if ($item.Length -ne $expectedSize) { throw "size mismatch $($item.Length) != $expectedSize" }
$hash=(Get-FileHash -LiteralPath $tmp -Algorithm SHA256).Hash.ToLowerInvariant()
if ($hash -ne $expectedHash.ToLowerInvariant()) { throw "sha256 mismatch $hash != $expectedHash" }
Get-Process opsera -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
Start-Sleep -Milliseconds 700
Move-Item -Force $tmp $target
Start-Process -FilePath $target -WorkingDirectory $dir -WindowStyle Hidden
'@ | Set-Content -LiteralPath $script -Encoding UTF8; Start-Process -FilePath powershell.exe -ArgumentList @('-NoLogo','-NoProfile','-ExecutionPolicy','Bypass','-File',$script) -WindowStyle Hidden; 'deploy scheduled'`,
		psQuote(remoteExe),
		psQuote(remoteExe),
		psQuote(relayHTTPURL(host, "")),
		psQuote(manifest.FileID),
		manifest.PartCount,
		manifest.Size,
		psQuote(manifest.SHA256),
	)
	result, err := relayRequest(host, code, password, relayws.Message{
		Type:    "exec",
		ID:      newRelayTaskID(),
		Command: cmd,
	})
	if err != nil {
		return err
	}
	if result.Error != "" {
		return errors.New(result.Error + "\n" + result.Output)
	}
	return nil
}

func relayUploadChunked(host, code, password, localPath, remotePath string, allowLegacy bool) error {
	info, err := os.Stat(localPath)
	if err != nil {
		return err
	}
	wsErr := relayUploadViaWS(host, code, password, localPath, remotePath)
	if wsErr == nil {
		return nil
	}
	httpErr := relayUploadViaHTTP(host, code, password, localPath, remotePath)
	if httpErr == nil {
		return nil
	}
	if info.Size() > relayMultipartThreshold {
		if multipartErr := relayUploadMultipart(host, code, password, localPath, remotePath); multipartErr != nil {
			if !allowLegacy {
				return fmt.Errorf("websocket upload failed: %v; relay-http upload failed: %v; multipart upload failed: %v", wsErr, httpErr, multipartErr)
			}
			if legacyErr := relayUploadLegacy(host, code, password, localPath, remotePath); legacyErr != nil {
				return fmt.Errorf("websocket upload failed: %v; relay-http upload failed: %v; multipart upload failed: %v; legacy upload failed: %v", wsErr, httpErr, multipartErr, legacyErr)
			}
			return nil
		}
		return nil
	}
	if !allowLegacy {
		return fmt.Errorf("websocket upload failed: %v; relay-http upload failed: %v", wsErr, httpErr)
	}
	if legacyErr := relayUploadLegacy(host, code, password, localPath, remotePath); legacyErr != nil {
		return fmt.Errorf("websocket upload failed: %v; relay-http upload failed: %v; legacy upload failed: %v", wsErr, httpErr, legacyErr)
	}
	return nil
}

func relayUploadMultipart(host, code, password, localPath, remotePath string) error {
	info, err := os.Stat(localPath)
	if err != nil {
		return err
	}
	totalHash, err := relayFileSHA256(localPath)
	if err != nil {
		return err
	}
	partCount := int((info.Size() + relayMultipartPartSize - 1) / relayMultipartPartSize)
	localDir, err := os.MkdirTemp("", "opsera-relay-parts-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(localDir)
	remoteDir := remotePath + ".parts"
	if err := relayExecChecked(host, code, password, fmt.Sprintf(
		`$ErrorActionPreference='Stop'; New-Item -ItemType Directory -Force -Path %s | Out-Null; Remove-Item -LiteralPath (Join-Path %s '*.tmp') -Force -ErrorAction SilentlyContinue; 'ready'`,
		psQuote(remoteDir), psQuote(remoteDir),
	)); err != nil {
		return err
	}
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, relayMultipartPartSize)
	for index := 0; index < partCount; index++ {
		n, readErr := io.ReadFull(f, buf)
		if errors.Is(readErr, io.ErrUnexpectedEOF) || errors.Is(readErr, io.EOF) {
			readErr = nil
		}
		if readErr != nil {
			return readErr
		}
		partName := fmt.Sprintf("part-%06d.bin", index)
		localPart := filepath.Join(localDir, partName)
		if err := os.WriteFile(localPart, buf[:n], 0o600); err != nil {
			return err
		}
		partHash, err := relayFileSHA256(localPart)
		if err != nil {
			return err
		}
		remotePart := remoteDir + `\` + partName
		matches, err := relayRemoteFileMatches(host, code, password, remotePart, int64(n), partHash)
		if err != nil {
			return err
		}
		if matches {
			fmt.Fprintf(os.Stderr, "relay multipart skip %d/%d %s\n", index+1, partCount, partName)
			continue
		}
		fmt.Fprintf(os.Stderr, "relay multipart upload %d/%d %s\n", index+1, partCount, partName)
		var uploadErr error
		for attempt := 0; attempt < 3; attempt++ {
			_ = relayExecChecked(host, code, password, fmt.Sprintf(`Remove-Item -LiteralPath %s -Force -ErrorAction SilentlyContinue; Remove-Item -LiteralPath %s -Force -ErrorAction SilentlyContinue; 'removed'`, psQuote(remotePart), psQuote(remotePart+".opsera-new")))
			uploadErr = relayUploadViaWS(host, code, password, localPart, remotePart)
			if uploadErr != nil {
				uploadErr = relayUploadViaHTTP(host, code, password, localPart, remotePart)
			}
			if uploadErr == nil {
				ok, err := relayRemoteFileMatches(host, code, password, remotePart, int64(n), partHash)
				if err != nil {
					return err
				}
				if ok {
					break
				} else {
					_ = relayExecChecked(host, code, password, fmt.Sprintf(`Remove-Item -LiteralPath %s -Force -ErrorAction SilentlyContinue; 'removed'`, psQuote(remotePart)))
					uploadErr = fmt.Errorf("remote part verification failed: %s", partName)
				}
			}
			time.Sleep(time.Duration(attempt+1) * time.Second)
		}
		if uploadErr != nil {
			return fmt.Errorf("upload part %s failed: %w", partName, uploadErr)
		}
	}
	return relayMergeMultipart(host, code, password, remoteDir, remotePath, partCount, info.Size(), totalHash)
}

func relayRemoteFileMatches(host, code, password, path string, size int64, sha string) (bool, error) {
	cmd := fmt.Sprintf(
		`$p=%s; if(Test-Path -LiteralPath $p){ $i=Get-Item -LiteralPath $p; $h=(Get-FileHash -LiteralPath $p -Algorithm SHA256).Hash; if($i.Length -eq %d -and $h -eq %s){ 'ok' } else { 'bad|' + $i.Length + '|' + $h } } else { 'missing' }`,
		psQuote(path), size, psQuote(sha),
	)
	result, err := relayRequestTimeout(host, code, password, relayws.Message{
		Type:    "exec",
		ID:      newRelayTaskID(),
		Command: cmd,
	}, 2*time.Minute)
	if err != nil {
		return false, err
	}
	if result.Error != "" {
		return false, errors.New(result.Error + "\n" + result.Output)
	}
	return strings.Contains(result.Output, "ok"), nil
}

func relayMergeMultipart(host, code, password, remoteDir, remotePath string, partCount int, totalSize int64, totalHash string) error {
	cmd := fmt.Sprintf(`$ErrorActionPreference='Stop';
$dir=%s; $target=%s; $expectedCount=%d; $expectedSize=%d; $expectedHash=%s;
$parts=Get-ChildItem -LiteralPath $dir -Filter 'part-*.bin' | Sort-Object Name;
if($parts.Count -ne $expectedCount){ throw "part count mismatch $($parts.Count) != $expectedCount" }
$tmp=$target + '.opsera-new';
Remove-Item -LiteralPath $tmp -Force -ErrorAction SilentlyContinue;
$out=[IO.File]::Open($tmp,[IO.FileMode]::Create,[IO.FileAccess]::Write);
try {
  foreach($part in $parts){
    $bytes=[IO.File]::ReadAllBytes($part.FullName);
    $out.Write($bytes,0,$bytes.Length);
  }
} finally { $out.Close() }
$item=Get-Item -LiteralPath $tmp;
$hash=(Get-FileHash -LiteralPath $tmp -Algorithm SHA256).Hash;
if($item.Length -ne $expectedSize){ throw "size mismatch $($item.Length) != $expectedSize" }
if($hash -ne $expectedHash){ throw "sha256 mismatch $hash != $expectedHash" }
Move-Item -Force $tmp $target;
Remove-Item -LiteralPath $dir -Recurse -Force -ErrorAction SilentlyContinue;
'merged'`,
		psQuote(remoteDir), psQuote(remotePath), partCount, totalSize, psQuote(totalHash),
	)
	return relayExecChecked(host, code, password, cmd)
}

func relayExecChecked(host, code, password, command string) error {
	result, err := relayRequestTimeout(host, code, password, relayws.Message{
		Type:    "exec",
		ID:      newRelayTaskID(),
		Command: command,
	}, 10*time.Minute)
	if err != nil {
		return err
	}
	if result.Error != "" {
		return errors.New(result.Error + "\n" + result.Output)
	}
	return nil
}

func relayUploadViaWS(host, code, password, localPath, remotePath string) error {
	info, err := os.Stat(localPath)
	if err != nil {
		return err
	}
	sum, err := relayFileSHA256(localPath)
	if err != nil {
		return err
	}
	taskID := newRelayTaskID()
	controlURL := relayControlURL(host, code, password)
	control, err := relayws.Dial(controlURL)
	if err != nil {
		return err
	}
	defer control.Close()
	if err := control.WriteJSON(relayws.Message{
		Type:   "file-upload",
		ID:     taskID,
		Path:   remotePath,
		SHA256: sum,
		Size:   info.Size(),
	}); err != nil {
		return err
	}
	fileConn, err := relayws.Dial(relayFileControlURL(host, code, password, taskID))
	if err != nil {
		return err
	}
	_ = fileConn.SetDeadline(time.Now().Add(relayFileTransferTimeout(info.Size())))
	type readResult struct {
		msg relayws.Message
		err error
	}
	resultCh := make(chan readResult, 1)
	go func() {
		var result relayws.Message
		err := control.ReadJSON(&result)
		resultCh <- readResult{msg: result, err: err}
	}()
	select {
	case item := <-resultCh:
		if item.err != nil {
			return item.err
		}
		if item.msg.Error != "" {
			return errors.New(item.msg.Error + "\n" + item.msg.Output)
		}
		if item.msg.Type != "accepted" {
			return fmt.Errorf("relay upload was not accepted: %s", item.msg.Type)
		}
	case <-time.After(30 * time.Second):
		return fmt.Errorf("relay upload accept timeout")
	}
	resultCh = make(chan readResult, 1)
	transferCh := make(chan error, 1)
	go func() {
		transferCh <- uploadFileWS(fileConn, localPath)
	}()
	select {
	case item := <-resultCh:
		_ = fileConn.Close()
		if item.err != nil {
			return item.err
		}
		if item.msg.Error != "" {
			return errors.New(item.msg.Error + "\n" + item.msg.Output)
		}
		return fmt.Errorf("relay upload finished before file transfer")
	case transferErr := <-transferCh:
		if transferErr != nil {
			return transferErr
		}
		return nil
	case <-time.After(relayFileTransferTimeout(info.Size())):
		_ = fileConn.Close()
		return fmt.Errorf("relay file transfer timeout")
	}
}

type relayFileFrame struct {
	Type  string `json:"type"`
	Index int64  `json:"index,omitempty"`
	Error string `json:"error,omitempty"`
}

func uploadFileWS(conn *relayws.Conn, localPath string) error {
	defer conn.Close()
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	var ready relayFileFrame
	if err := conn.ReadJSON(&ready); err != nil {
		return err
	}
	if ready.Type != "ready" {
		return fmt.Errorf("invalid upload ready response: %s", ready.Type)
	}
	_ = conn.SetDeadline(time.Time{})
	resultCh := make(chan error, 1)
	go func() {
		for {
			var frame relayFileFrame
			if err := conn.ReadJSON(&frame); err != nil {
				resultCh <- err
				return
			}
			switch frame.Type {
			case "ack":
				continue
			case "complete":
				resultCh <- nil
				return
			case "error":
				resultCh <- errors.New(frame.Error)
				return
			default:
				resultCh <- fmt.Errorf("invalid upload response: %s", frame.Type)
				return
			}
		}
	}()
	const chunkSize = 32 * 1024
	buf := make([]byte, chunkSize)
	frame := make([]byte, 8+chunkSize)
	for index := int64(0); ; index++ {
		n, readErr := io.ReadFull(f, buf)
		if errors.Is(readErr, io.ErrUnexpectedEOF) || errors.Is(readErr, io.EOF) {
			readErr = nil
		}
		if readErr != nil {
			return readErr
		}
		if n == 0 {
			break
		}
		binary.BigEndian.PutUint64(frame[:8], uint64(index))
		copy(frame[8:], buf[:n])
		if err := conn.WriteBinary(frame[:8+n]); err != nil {
			return err
		}
		select {
		case err := <-resultCh:
			if err != nil {
				return err
			}
			return fmt.Errorf("upload completed before local file was fully sent")
		default:
		}
	}
	if err := conn.WriteJSON(relayFileFrame{Type: "done"}); err != nil {
		return err
	}
	return <-resultCh
}

func relayFileTransferTimeout(size int64) time.Duration {
	timeout := 2 * time.Minute
	perMB := time.Duration((size+(1<<20)-1)/(1<<20)) * 30 * time.Second
	if perMB > timeout {
		timeout = perMB
	}
	if timeout > 30*time.Minute {
		return 30 * time.Minute
	}
	return timeout
}

func relayUploadViaHTTP(host, code, password, localPath, remotePath string) error {
	manifest, err := uploadFileToRelay(host, localPath)
	if err != nil {
		return err
	}
	result, err := relayRequestTimeout(host, code, password, relayws.Message{
		Type:      "download-file",
		ID:        newRelayTaskID(),
		Path:      remotePath,
		FileID:    manifest.FileID,
		SHA256:    manifest.SHA256,
		Size:      manifest.Size,
		PartSize:  manifest.PartSize,
		PartCount: manifest.PartCount,
		RelayURL:  relayHTTPURL(host, ""),
	}, relayFileTransferTimeout(manifest.Size))
	if err != nil {
		return err
	}
	if result.Error != "" {
		return errors.New(result.Error + "\n" + result.Output)
	}
	return nil
}

type relayFileManifest struct {
	FileID    string `json:"fileId"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	PartSize  int64  `json:"partSize"`
	PartCount int    `json:"partCount"`
}

func uploadFileToRelay(host, localPath string) (relayFileManifest, error) {
	info, err := os.Stat(localPath)
	if err != nil {
		return relayFileManifest{}, err
	}
	const partSize int64 = 1024 * 1024
	sum, err := relayFileSHA256(localPath)
	if err != nil {
		return relayFileManifest{}, err
	}
	manifest := relayFileManifest{
		FileID:    newRelayTaskID(),
		Name:      filepath.Base(localPath),
		Size:      info.Size(),
		SHA256:    sum,
		PartSize:  partSize,
		PartCount: int((info.Size() + partSize - 1) / partSize),
	}
	if manifest.PartCount == 0 {
		manifest.PartCount = 1
	}
	raw, _ := json.Marshal(manifest)
	resp, err := relayHTTPClientLong().Post(relayHTTPURL(host, "/files/init"), "application/json", bytes.NewReader(raw))
	if err != nil {
		return relayFileManifest{}, err
	}
	if err := closeRelayResponse(resp); err != nil {
		return relayFileManifest{}, err
	}
	f, err := os.Open(localPath)
	if err != nil {
		return relayFileManifest{}, err
	}
	defer f.Close()
	buf := make([]byte, partSize)
	for index := 0; index < manifest.PartCount; index++ {
		n, readErr := io.ReadFull(f, buf)
		if errors.Is(readErr, io.ErrUnexpectedEOF) || errors.Is(readErr, io.EOF) {
			readErr = nil
		}
		if readErr != nil {
			return relayFileManifest{}, readErr
		}
		if n == 0 && info.Size() > 0 {
			return relayFileManifest{}, fmt.Errorf("unexpected empty part %d", index)
		}
		if err := putRelayPart(host, manifest.FileID, index, buf[:n]); err != nil {
			return relayFileManifest{}, err
		}
	}
	return manifest, nil
}

func putRelayPart(host, fileID string, index int, data []byte) error {
	url := relayHTTPURL(host, fmt.Sprintf("/files/%s/parts/%d", fileID, index))
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	resp, err := relayHTTPClientLong().Do(req)
	if err != nil {
		return err
	}
	return closeRelayResponse(resp)
}

func relayFileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func closeRelayResponse(resp *http.Response) error {
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("relay http failed: %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func relayHTTPClientLong() *http.Client {
	return &http.Client{Timeout: 5 * time.Minute}
}

func relayUploadLegacy(host, code, password, localPath, remotePath string) error {
	if _, err := relayRequest(host, code, password, relayws.Message{
		Type:    "exec",
		ID:      newRelayTaskID(),
		Command: "Remove-Item -LiteralPath " + psQuote(remotePath) + " -Force -ErrorAction SilentlyContinue",
	}); err != nil {
		return err
	}
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 256*1024)
	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			result, err := relayRequest(host, code, password, relayws.Message{
				Type: "upload-append",
				ID:   newRelayTaskID(),
				Path: remotePath,
				Data: base64.StdEncoding.EncodeToString(buf[:n]),
			})
			if err != nil {
				return err
			}
			if result.Error != "" {
				return errors.New(result.Error)
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	return nil
}

func relayRequest(host, code, password string, msg relayws.Message) (relayws.Message, error) {
	return relayRequestTimeout(host, code, password, msg, 2*time.Minute)
}

func relayRequestTimeout(host, code, password string, msg relayws.Message, timeout time.Duration) (relayws.Message, error) {
	wsURL := relayControlURL(host, code, password)
	conn, err := relayws.Dial(wsURL)
	if err != nil {
		return relayws.Message{}, err
	}
	defer conn.Close()
	if err := conn.WriteJSON(msg); err != nil {
		return relayws.Message{}, err
	}
	type readResult struct {
		msg relayws.Message
		err error
	}
	ch := make(chan readResult, 1)
	go func() {
		var result relayws.Message
		err := conn.ReadJSON(&result)
		ch <- readResult{msg: result, err: err}
	}()
	select {
	case item := <-ch:
		if item.err != nil {
			return relayws.Message{}, item.err
		}
		return item.msg, nil
	case <-time.After(timeout):
		return relayws.Message{}, fmt.Errorf("relay task timeout after %s", timeout)
	}
}

func relayRequestNoTimeout(host, code, password string, msg relayws.Message) (relayws.Message, error) {
	wsURL := relayControlURL(host, code, password)
	conn, err := relayws.Dial(wsURL)
	if err != nil {
		return relayws.Message{}, err
	}
	defer conn.Close()
	if err := conn.WriteJSON(msg); err != nil {
		return relayws.Message{}, err
	}
	var result relayws.Message
	if err := conn.ReadJSON(&result); err != nil {
		return relayws.Message{}, err
	}
	if result.Error != "" {
		return result, nil
	}
	return result, nil
}

func relayVerify(host, code, password string) (map[string]any, error) {
	rawURL := relayHTTPURL(host, "/devices/verify")
	body := strings.NewReader(fmt.Sprintf(`{"machineCode":%q,"accessCode":%q}`, code, password))
	resp, err := httpClient().Post(rawURL, "application/json", body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("relay verify failed: %s", resp.Status)
	}
	var out map[string]any
	return out, json.NewDecoder(resp.Body).Decode(&out)
}

func httpClient() *http.Client {
	return &http.Client{Timeout: 15 * time.Second}
}

func relayControlURL(host, code, password string) string {
	values := url.Values{}
	values.Set("machineCode", code)
	values.Set("accessCode", password)
	return relayWSURLFromHost(host, "/control/ws") + "?" + values.Encode()
}

func relayFileControlURL(host, code, password, taskID string) string {
	values := url.Values{}
	values.Set("machineCode", code)
	values.Set("accessCode", password)
	values.Set("taskId", taskID)
	return relayWSURLFromHost(host, "/control/file/ws") + "?" + values.Encode()
}

func relayHTTPURL(host, path string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		host = defaultRelayHost
	}
	if strings.HasPrefix(host, "http://") || strings.HasPrefix(host, "https://") {
		return strings.TrimRight(host, "/") + path
	}
	return "https://" + strings.TrimRight(host, "/") + path
}

func relayWSURLFromHost(host, path string) string {
	httpURL := relayHTTPURL(host, path)
	if strings.HasPrefix(httpURL, "https://") {
		return "wss://" + strings.TrimPrefix(httpURL, "https://")
	}
	return "ws://" + strings.TrimPrefix(httpURL, "http://")
}

func relayArgHost(args []string, index int) string {
	if len(args) > index {
		return args[index]
	}
	return defaultRelayHost
}

func isRelayHostArg(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://") || strings.HasPrefix(value, "ws://") || strings.HasPrefix(value, "wss://") {
		return true
	}
	if strings.ContainsAny(value, " \t\r\n'\";$(){}[]|&<>") {
		return false
	}
	return strings.Contains(value, ".")
}

func newRelayTaskID() string {
	return fmt.Sprintf("relay-%d", time.Now().UnixNano())
}

func psQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
