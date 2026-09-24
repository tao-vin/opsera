package session

import (
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var ipv4Pattern = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`)

func LaunchTarget(args []string) string {
	fallback := ""
	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		if arg == "" {
			continue
		}
		if isTargetFlag(arg) {
			if i+1 < len(args) {
				return normalizeTarget(args[i+1])
			}
			return ""
		}
		if value, ok := splitTargetAssignment(arg); ok {
			return normalizeTarget(value)
		}
		if strings.HasPrefix(arg, "opsera://") {
			return normalizeTarget(strings.TrimPrefix(arg, "opsera://"))
		}
	}
	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		if strings.EqualFold(arg, "-newtab") || strings.EqualFold(arg, "/newtab") {
			if i+1 < len(args) {
				return normalizeTarget(args[i+1])
			}
			return ""
		}
	}
	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		if strings.EqualFold(arg, "-url") || strings.EqualFold(arg, "/url") {
			if i+1 < len(args) {
				return normalizeTarget(args[i+1])
			}
			return ""
		}
	}
	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		if arg == "" {
			continue
		}
		if isIgnoredLaunchArg(arg) {
			return ""
		}
		if isXshellValueFlag(arg) {
			i++
			continue
		}
		if fallback == "" && !strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "/") {
			fallback = normalizeTarget(arg)
		}
	}
	for _, arg := range args {
		if ip := ipv4Pattern.FindString(strings.TrimSpace(arg)); ip != "" {
			return ip
		}
	}
	if fallback != "" {
		return fallback
	}
	return ""
}

func isTargetFlag(arg string) bool {
	arg = strings.ToLower(strings.TrimSpace(arg))
	switch arg {
	case "--target", "-target", "/target", "--host", "-host", "/host", "--server", "-server", "/server":
		return true
	default:
		return false
	}
}

func splitTargetAssignment(arg string) (string, bool) {
	arg = strings.TrimSpace(arg)
	lower := strings.ToLower(arg)
	for _, prefix := range []string{"--target=", "-target=", "/target=", "target=", "--host=", "-host=", "/host=", "host=", "--server=", "-server=", "/server=", "server="} {
		if strings.HasPrefix(lower, prefix) {
			return arg[len(prefix):], true
		}
	}
	return "", false
}

func isXshellValueFlag(arg string) bool {
	return strings.EqualFold(arg, "-url") ||
		strings.EqualFold(arg, "/url") ||
		strings.EqualFold(arg, "-newtab") ||
		strings.EqualFold(arg, "/newtab") ||
		strings.EqualFold(arg, "-setviewer") ||
		strings.EqualFold(arg, "/setviewer")
}

func isIgnoredLaunchArg(arg string) bool {
	arg = strings.ToLower(strings.TrimSpace(arg))
	switch arg {
	case "help", "--help", "-h", "/h", "/?", "version", "--version", "-version", "/version":
		return true
	default:
		return false
	}
}

func normalizeTarget(value string) string {
	value = strings.Trim(strings.TrimSpace(value), `"`)
	if value == "" {
		return ""
	}
	base := filepath.Base(value)
	if ip := ipv4Pattern.FindString(base); ip != "" {
		return ip
	}
	if ip := ipv4Pattern.FindString(value); ip != "" {
		return ip
	}
	base = strings.TrimSuffix(base, filepath.Ext(base))
	if at := strings.LastIndex(base, "@"); at >= 0 && at+1 < len(base) {
		return strings.TrimSpace(base[at+1:])
	}
	return value
}

func XshellSessionTargetFromPath(path string) string {
	path = strings.Trim(strings.TrimSpace(path), `"`)
	if path == "" {
		return ""
	}
	name := filepath.Base(path)
	if !strings.HasSuffix(strings.ToLower(name), ".xsh") {
		return ""
	}
	return normalizeTarget(strings.TrimSuffix(name, filepath.Ext(name)))
}

func LatestXshellSessionTarget(paths []string, modTime func(string) (time.Time, error), maxAge time.Duration, now time.Time) string {
	bestPath := ""
	bestMod := time.Time{}
	for _, path := range paths {
		target := XshellSessionTargetFromPath(path)
		if target == "" {
			continue
		}
		mod, err := modTime(path)
		if err != nil {
			continue
		}
		if maxAge > 0 && now.Sub(mod) > maxAge {
			continue
		}
		if bestPath == "" || mod.After(bestMod) {
			bestPath = path
			bestMod = mod
		}
	}
	return XshellSessionTargetFromPath(bestPath)
}
