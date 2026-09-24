//go:build windows

package app

import (
	"path/filepath"
	"syscall"
	"time"
	"unsafe"
)

const (
	nimAdd         = 0x00000000
	nimDelete      = 0x00000002
	nifMessage     = 0x00000001
	nifIcon        = 0x00000002
	nifTip         = 0x00000004
	imageIcon      = 1
	lrLoadFromFile = 0x00000010
	lrDefaultSize  = 0x00000040
	wmSetIcon      = 0x0080
	iconSmall      = 0
	iconBig        = 1
)

var (
	shell32         = syscall.NewLazyDLL("shell32.dll")
	user32          = syscall.NewLazyDLL("user32.dll")
	procShellNotify = shell32.NewProc("Shell_NotifyIconW")
	procLoadImage   = user32.NewProc("LoadImageW")
	procFindWindow  = user32.NewProc("FindWindowW")
	procSendMessage = user32.NewProc("SendMessageW")
)

type notifyIconData struct {
	cbSize           uint32
	hWnd             syscall.Handle
	uID              uint32
	uFlags           uint32
	uCallbackMessage uint32
	hIcon            syscall.Handle
	szTip            [128]uint16
	dwState          uint32
	dwStateMask      uint32
	szInfo           [256]uint16
	uVersion         uint32
	szInfoTitle      [64]uint16
	dwInfoFlags      uint32
	guidItem         [16]byte
	hBalloonIcon     syscall.Handle
}

func startTrayIcon(dataDir string) func() {
	iconPath := filepath.Join(dataDir, "opsera.ico")
	icon, err := loadIcon(iconPath)
	if err != nil || icon == 0 {
		return func() {}
	}
	go applyWindowIcon(icon)
	nid := notifyIconData{
		cbSize: uint32(unsafe.Sizeof(notifyIconData{})),
		uID:    1,
		uFlags: nifIcon | nifTip | nifMessage,
		hIcon:  icon,
	}
	copy(nid.szTip[:], syscall.StringToUTF16("Opsera"))
	procShellNotify.Call(nimAdd, uintptr(unsafe.Pointer(&nid)))
	return func() {
		procShellNotify.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))
	}
}

func applyWindowIcon(icon syscall.Handle) {
	title, _ := syscall.UTF16PtrFromString("Opsera")
	for i := 0; i < 20; i++ {
		hwnd, _, _ := procFindWindow.Call(0, uintptr(unsafe.Pointer(title)))
		if hwnd != 0 {
			procSendMessage.Call(hwnd, wmSetIcon, iconSmall, uintptr(icon))
			procSendMessage.Call(hwnd, wmSetIcon, iconBig, uintptr(icon))
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func loadIcon(path string) (syscall.Handle, error) {
	ptr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	handle, _, callErr := procLoadImage.Call(0, uintptr(unsafe.Pointer(ptr)), imageIcon, 0, 0, lrLoadFromFile|lrDefaultSize)
	if handle == 0 {
		return 0, callErr
	}
	return syscall.Handle(handle), nil
}
