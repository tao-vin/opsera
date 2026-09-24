//go:build !windows

package app

func startTrayIcon(dataDir string) func() {
	return func() {}
}
