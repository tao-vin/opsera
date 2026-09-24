//go:build !windows

package app

import (
	"os/exec"
	"path/filepath"
)

func startDetached(exe string) error {
	cmd := exec.Command(exe)
	cmd.Dir = filepath.Dir(exe)
	return cmd.Start()
}

func hideCommandWindow(_ *exec.Cmd) {}
