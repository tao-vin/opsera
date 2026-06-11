package session

import (
	"path/filepath"
	"regexp"
	"strings"
)

var ipv4Pattern = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`)

func LaunchTarget(args []string) string {
	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		if arg == "" {
			continue
		}
		if arg == "--target" || arg == "-target" || arg == "/target" {
			if i+1 < len(args) {
				return normalizeTarget(args[i+1])
			}
			return ""
		}
		if strings.HasPrefix(arg, "--target=") {
			return normalizeTarget(strings.TrimPrefix(arg, "--target="))
		}
		if strings.HasPrefix(arg, "opsera://") {
			return normalizeTarget(strings.TrimPrefix(arg, "opsera://"))
		}
		if !strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "/") {
			return normalizeTarget(arg)
		}
	}
	return ""
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
