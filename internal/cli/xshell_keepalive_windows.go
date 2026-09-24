package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const keepAliveWorkerEnv = "OPSERA_XSHELL_KEEPALIVE_WORKER"

var (
	keepAliveUser32                  = syscall.NewLazyDLL("user32.dll")
	keepAliveKernel32                = syscall.NewLazyDLL("kernel32.dll")
	keepAliveProcCreateMutexW        = keepAliveKernel32.NewProc("CreateMutexW")
	keepAliveProcOpenMutexW          = keepAliveKernel32.NewProc("OpenMutexW")
	keepAliveProcCloseHandle         = keepAliveKernel32.NewProc("CloseHandle")
	keepAliveProcGetLastError        = keepAliveKernel32.NewProc("GetLastError")
	keepAliveProcEnumWindows         = keepAliveUser32.NewProc("EnumWindows")
	keepAliveProcGetForegroundWindow = keepAliveUser32.NewProc("GetForegroundWindow")
	keepAliveProcGetAsyncKeyState    = keepAliveUser32.NewProc("GetAsyncKeyState")
	keepAliveProcGetCursorPos        = keepAliveUser32.NewProc("GetCursorPos")
	keepAliveProcGetWindowRect       = keepAliveUser32.NewProc("GetWindowRect")
	keepAliveProcGetWindowTextW      = keepAliveUser32.NewProc("GetWindowTextW")
	keepAliveProcGetWindowTextLength = keepAliveUser32.NewProc("GetWindowTextLengthW")
	keepAliveProcIsWindow            = keepAliveUser32.NewProc("IsWindow")
	keepAliveProcIsWindowVisible     = keepAliveUser32.NewProc("IsWindowVisible")
	keepAliveProcMapVirtualKeyW      = keepAliveUser32.NewProc("MapVirtualKeyW")
	keepAliveProcSendInput           = keepAliveUser32.NewProc("SendInput")
	keepAliveProcSetForegroundWindow = keepAliveUser32.NewProc("SetForegroundWindow")
	keepAliveProcSetCursorPos        = keepAliveUser32.NewProc("SetCursorPos")
	keepAliveProcShowWindowAsync     = keepAliveUser32.NewProc("ShowWindowAsync")
	keepAliveProcKeybdEvent          = keepAliveUser32.NewProc("keybd_event")
)

const (
	keepAliveErrorAlreadyExists  = 183
	keepAliveInputKeyboard       = 1
	keepAliveKeyEventFKeyUp      = 0x0002
	keepAliveMouseEventFLeftDown = 0x0002
	keepAliveMouseEventFLeftUp   = 0x0004
	keepAliveSWRestore           = 9
	keepAliveVKReturn            = 0x0D
	keepAliveVKShift             = 0x10
	keepAliveVKControl           = 0x11
	keepAliveVKMenu              = 0x12
	keepAliveMapvkVkToVsc        = 0
	keepAliveSynchronize         = 0x00100000
)

type xshellKeepAliveStatus struct {
	Running         bool   `json:"running"`
	IntervalSeconds int    `json:"intervalSeconds"`
	LogPath         string `json:"logPath"`
	LastLine        string `json:"lastLine,omitempty"`
	LastSeenSeconds int64  `json:"lastSeenSeconds,omitempty"`
}

type keepAliveKeyboardInput struct {
	Vk        uint16
	Scan      uint16
	Flags     uint32
	Time      uint32
	ExtraInfo uintptr
}

type keepAliveInput struct {
	Type    uint32
	Padding uint32
	Ki      keepAliveKeyboardInput
	_       [8]byte
}

type keepAliveMouseInput struct {
	Dx        int32
	Dy        int32
	MouseData uint32
	Flags     uint32
	Time      uint32
	ExtraInfo uintptr
}

type keepAliveMouseEventInput struct {
	Type    uint32
	Padding uint32
	Mi      keepAliveMouseInput
}

type keepAlivePoint struct {
	X int32
	Y int32
}

type keepAliveRect struct {
	Left   int32
	Top    int32
	Right  int32
	Bottom int32
}

func runXshellKeepAlive(args []string) (bool, error) {
	if len(args) == 1 && strings.EqualFold(args[0], "status") {
		return true, writeJSON(xshellKeepAliveStatusSnapshot())
	}
	if len(args) > 0 {
		return true, fmt.Errorf("usage: opsera xshell keepalive [status]")
	}
	if os.Getenv(keepAliveWorkerEnv) != "1" {
		if xshellKeepAliveMutexExists() {
			return true, nil
		}
		return true, xshellKeepAliveStartWorker()
	}
	if !xshellKeepAliveSingleInstance() {
		return true, nil
	}
	interval := xshellKeepAliveInterval()
	xshellKeepAliveLogLine(fmt.Sprintf("started: interval=%s", interval))
	for {
		xshellKeepAliveOnce()
		time.Sleep(interval)
	}
}

func xshellKeepAliveStartWorker() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "xshell", "keepalive")
	cmd.Dir = filepath.Dir(exe)
	cmd.Env = append(os.Environ(), keepAliveWorkerEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	xshellKeepAliveLogLine(fmt.Sprintf("spawned: pid=%d", cmd.Process.Pid))
	return nil
}

func xshellKeepAliveInterval() time.Duration {
	value := strings.TrimSpace(os.Getenv("OPSERA_XSHELL_KEEPALIVE_INTERVAL_SECONDS"))
	if value == "" {
		return 3 * time.Minute
	}
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds < 10 {
		return 3 * time.Minute
	}
	return time.Duration(seconds) * time.Second
}

func xshellKeepAliveSingleInstance() bool {
	name, _ := syscall.UTF16PtrFromString(`Global\OpseraXshellKeepAlive`)
	keepAliveProcCreateMutexW.Call(0, 1, uintptr(unsafe.Pointer(name)))
	last, _, _ := keepAliveProcGetLastError.Call()
	return last != keepAliveErrorAlreadyExists
}

func xshellKeepAliveStatusSnapshot() xshellKeepAliveStatus {
	logPath := xshellKeepAliveLogPath()
	lastLine := xshellKeepAliveLastLine(logPath)
	status := xshellKeepAliveStatus{
		Running:         xshellKeepAliveMutexExists(),
		IntervalSeconds: int(xshellKeepAliveInterval().Seconds()),
		LogPath:         logPath,
		LastLine:        lastLine,
	}
	if len(lastLine) >= len("2006-01-02 15:04:05") {
		if t, err := time.ParseInLocation("2006-01-02 15:04:05", lastLine[:len("2006-01-02 15:04:05")], time.Local); err == nil {
			status.LastSeenSeconds = int64(time.Since(t).Seconds())
		}
	}
	return status
}

func xshellKeepAliveMutexExists() bool {
	name, _ := syscall.UTF16PtrFromString(`Global\OpseraXshellKeepAlive`)
	handle, _, _ := keepAliveProcOpenMutexW.Call(keepAliveSynchronize, 0, uintptr(unsafe.Pointer(name)))
	if handle == 0 {
		return false
	}
	keepAliveProcCloseHandle.Call(handle)
	return true
}

func xshellKeepAliveOnce() {
	hwnd := xshellKeepAliveFindWindow()
	if hwnd == 0 {
		xshellKeepAliveLogLine("skip: xshell window not found")
		return
	}
	title := xshellKeepAliveWindowText(hwnd)
	if xshellKeepAliveModifierKeyDown() {
		xshellKeepAliveLogLine("skip: user modifier key is down")
		return
	}
	previous := xshellKeepAliveForegroundWindow()
	keepAliveProcShowWindowAsync.Call(hwnd, keepAliveSWRestore)
	time.Sleep(150 * time.Millisecond)
	keepAliveProcSetForegroundWindow.Call(hwnd)
	time.Sleep(250 * time.Millisecond)
	if xshellKeepAliveForegroundWindow() != hwnd {
		xshellKeepAliveLogLine("warn: foreground activation not confirmed, trying terminal click: " + title)
	}
	xshellKeepAliveFocusTerminal(hwnd)
	if !xshellKeepAlivePressEnter() {
		xshellKeepAliveLogLine("failed: SendInput did not accept Enter: " + title)
		return
	}
	xshellKeepAliveLogLine("sent: enter: " + title)
	time.Sleep(600 * time.Millisecond)
	if previous != 0 && previous != hwnd && xshellKeepAliveIsWindow(previous) {
		keepAliveProcSetForegroundWindow.Call(previous)
	}
}

func xshellKeepAliveFindWindow() uintptr {
	var exact uintptr
	var fallback uintptr
	cb := syscall.NewCallback(func(hwnd uintptr, lparam uintptr) uintptr {
		if exact != 0 || !xshellKeepAliveWindowVisible(hwnd) {
			return 1
		}
		title := xshellKeepAliveWindowText(hwnd)
		if strings.Contains(title, "GateShell - Xshell") {
			exact = hwnd
			return 1
		}
		if fallback == 0 && strings.Contains(title, " - Xshell") {
			fallback = hwnd
		}
		return 1
	})
	keepAliveProcEnumWindows.Call(cb, 0)
	if exact != 0 {
		return exact
	}
	return fallback
}

func xshellKeepAlivePressEnter() bool {
	scan, _, _ := keepAliveProcMapVirtualKeyW.Call(uintptr(keepAliveVKReturn), keepAliveMapvkVkToVsc)
	down := keepAliveInput{Type: keepAliveInputKeyboard, Ki: keepAliveKeyboardInput{Vk: keepAliveVKReturn, Scan: uint16(scan)}}
	up := keepAliveInput{Type: keepAliveInputKeyboard, Ki: keepAliveKeyboardInput{Vk: keepAliveVKReturn, Scan: uint16(scan), Flags: keepAliveKeyEventFKeyUp}}
	sent, _, _ := keepAliveProcSendInput.Call(1, uintptr(unsafe.Pointer(&down)), unsafe.Sizeof(down))
	if sent != 1 {
		return false
	}
	sent, _, _ = keepAliveProcSendInput.Call(1, uintptr(unsafe.Pointer(&up)), unsafe.Sizeof(up))
	if sent != 1 {
		return false
	}
	time.Sleep(20 * time.Millisecond)
	keepAliveProcKeybdEvent.Call(keepAliveVKReturn, scan, 0, 0)
	time.Sleep(20 * time.Millisecond)
	keepAliveProcKeybdEvent.Call(keepAliveVKReturn, scan, keepAliveKeyEventFKeyUp, 0)
	return true
}

func xshellKeepAliveFocusTerminal(hwnd uintptr) {
	var r keepAliveRect
	ok, _, _ := keepAliveProcGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	if ok == 0 {
		return
	}
	var old keepAlivePoint
	_, _, _ = keepAliveProcGetCursorPos.Call(uintptr(unsafe.Pointer(&old)))
	width := r.Right - r.Left
	height := r.Bottom - r.Top
	if width <= 0 || height <= 0 {
		return
	}
	x := r.Left + int32(float64(width)*0.68)
	y := r.Top + int32(float64(height)*0.48)
	keepAliveProcSetCursorPos.Call(uintptr(x), uintptr(y))
	time.Sleep(40 * time.Millisecond)
	xshellKeepAliveMouseClick()
	time.Sleep(120 * time.Millisecond)
	keepAliveProcSetCursorPos.Call(uintptr(old.X), uintptr(old.Y))
}

func xshellKeepAliveMouseClick() {
	inDown := keepAliveMouseEventInput{Type: 0, Mi: keepAliveMouseInput{Flags: keepAliveMouseEventFLeftDown}}
	inUp := keepAliveMouseEventInput{Type: 0, Mi: keepAliveMouseInput{Flags: keepAliveMouseEventFLeftUp}}
	keepAliveProcSendInput.Call(1, uintptr(unsafe.Pointer(&inDown)), unsafe.Sizeof(inDown))
	time.Sleep(30 * time.Millisecond)
	keepAliveProcSendInput.Call(1, uintptr(unsafe.Pointer(&inUp)), unsafe.Sizeof(inUp))
}

func xshellKeepAliveForegroundWindow() uintptr {
	hwnd, _, _ := keepAliveProcGetForegroundWindow.Call()
	return hwnd
}

func xshellKeepAliveModifierKeyDown() bool {
	return xshellKeepAliveKeyDown(keepAliveVKShift) || xshellKeepAliveKeyDown(keepAliveVKControl) || xshellKeepAliveKeyDown(keepAliveVKMenu)
}

func xshellKeepAliveKeyDown(vk uintptr) bool {
	state, _, _ := keepAliveProcGetAsyncKeyState.Call(vk)
	return state&0x8000 != 0
}

func xshellKeepAliveIsWindow(hwnd uintptr) bool {
	ok, _, _ := keepAliveProcIsWindow.Call(hwnd)
	return ok != 0
}

func xshellKeepAliveWindowVisible(hwnd uintptr) bool {
	ok, _, _ := keepAliveProcIsWindowVisible.Call(hwnd)
	return ok != 0
}

func xshellKeepAliveWindowText(hwnd uintptr) string {
	n, _, _ := keepAliveProcGetWindowTextLength.Call(hwnd)
	if n == 0 {
		return ""
	}
	buf := make([]uint16, n+1)
	keepAliveProcGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return syscall.UTF16ToString(buf)
}

func xshellKeepAliveLogLine(msg string) {
	path := xshellKeepAliveLogPath()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintf(f, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), msg)
}

func xshellKeepAliveLogPath() string {
	return filepath.Join(os.TempDir(), "opsera-xshell-keepalive.log")
}

func xshellKeepAliveLastLine(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(data), "\r\n"), "\n")
	if len(lines) == 0 {
		return ""
	}
	return strings.TrimRight(lines[len(lines)-1], "\r")
}
