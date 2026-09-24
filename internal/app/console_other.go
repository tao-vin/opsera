//go:build !windows

package app

func relaunchGUIIfNeeded() (bool, error) {
	return false, nil
}
