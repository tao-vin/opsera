//go:build windows

package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

var (
	consoleKernel32          = syscall.NewLazyDLL("kernel32.dll")
	consoleUser32            = syscall.NewLazyDLL("user32.dll")
	consoleProcGetConsoleWnd = consoleKernel32.NewProc("GetConsoleWindow")
	consoleProcShowWindow    = consoleUser32.NewProc("ShowWindow")
)

const consoleHideWindow = 0

func relaunchGUIIfNeeded() (bool, error) {
	if os.Getenv("OPSERA_GUI_DETACHED") == "1" {
		hideOwnConsoleWindow()
		return false, nil
	}
	hwnd, _, _ := consoleProcGetConsoleWnd.Call()
	if hwnd == 0 {
		return false, nil
	}
	exe, err := os.Executable()
	if err != nil {
		hideOwnConsoleWindow()
		return false, err
	}
	cmd := exec.Command(exe)
	cmd.Dir = filepath.Dir(exe)
	cmd.Env = append(os.Environ(), "OPSERA_GUI_DETACHED=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Start(); err != nil {
		hideOwnConsoleWindow()
		return false, err
	}
	return true, nil
}

func hideOwnConsoleWindow() {
	hwnd, _, _ := consoleProcGetConsoleWnd.Call()
	if hwnd == 0 {
		return
	}
	consoleProcShowWindow.Call(hwnd, consoleHideWindow)
}
