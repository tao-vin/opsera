//go:build windows

package app

import (
	"strings"
	"syscall"
	"unsafe"
)

var (
	activateUser32                    = syscall.NewLazyDLL("user32.dll")
	procActivateEnumWindows           = activateUser32.NewProc("EnumWindows")
	procActivateGetWindowThreadProcID = activateUser32.NewProc("GetWindowThreadProcessId")
	procActivateIsWindowVisible       = activateUser32.NewProc("IsWindowVisible")
	procActivateShowWindow            = activateUser32.NewProc("ShowWindow")
	procActivateSetForegroundWindow   = activateUser32.NewProc("SetForegroundWindow")
	procActivateGetWindowTextW        = activateUser32.NewProc("GetWindowTextW")
)

const swRestore = 9

func activateExistingWindow() {
	hwnd := findOpseraWindow()
	if hwnd == 0 {
		return
	}
	procActivateShowWindow.Call(hwnd, swRestore)
	procActivateSetForegroundWindow.Call(hwnd)
}

func findOpseraWindow() uintptr {
	currentPID := uint32(syscall.Getpid())
	var found uintptr
	cb := syscall.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
		if found != 0 {
			return 0
		}
		visible, _, _ := procActivateIsWindowVisible.Call(hwnd)
		if visible == 0 {
			return 1
		}
		var pid uint32
		procActivateGetWindowThreadProcID.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
		if pid != currentPID {
			return 1
		}
		title := windowTitle(hwnd)
		if strings.Contains(strings.ToLower(title), "opsera") {
			found = hwnd
			return 0
		}
		return 1
	})
	procActivateEnumWindows.Call(cb, 0)
	return found
}

func windowTitle(hwnd uintptr) string {
	buf := make([]uint16, 256)
	n, _, _ := procActivateGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf[:n])
}
