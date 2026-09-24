package session

import (
	"testing"
	"time"
)

func TestLaunchTargetPrefersXshellNewTabAsset(t *testing.T) {
	target := LaunchTarget([]string{
		"-url",
		"ssh://sso-user:token@203.0.113.10:60022",
		"-newtab",
		"10.0.0.9:22",
	})
	if target != "10.0.0.9" {
		t.Fatalf("target = %q", target)
	}
}

func TestLaunchTargetFallsBackToXshellURL(t *testing.T) {
	target := LaunchTarget([]string{
		"-url",
		"ssh://sso-user:token@203.0.113.10:60022",
	})
	if target != "203.0.113.10" {
		t.Fatalf("target = %q", target)
	}
}

func TestLaunchTargetParsesOpseraURL(t *testing.T) {
	target := LaunchTarget([]string{"opsera://192.168.70.10"})
	if target != "192.168.70.10" {
		t.Fatalf("target = %q", target)
	}
}

func TestLaunchTargetParsesBastionHostFlags(t *testing.T) {
	cases := [][]string{
		{"--host", "192.168.70.10"},
		{"--server=192.168.70.10"},
		{"/target=192.168.70.10"},
		{"launch", "target=192.168.70.10"},
	}
	for _, args := range cases {
		target := LaunchTarget(args)
		if target != "192.168.70.10" {
			t.Fatalf("LaunchTarget(%v) = %q", args, target)
		}
	}
}

func TestLaunchTargetIgnoresCliHelpAndVersion(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"version"}, {"--help"}, {"--version"}} {
		if target := LaunchTarget(args); target != "" {
			t.Fatalf("LaunchTarget(%v) = %q", args, target)
		}
	}
}

func TestXshellSessionTargetFromPath(t *testing.T) {
	path := `C:\Users\Administrator\AppData\Roaming\NetSarang\Xshell\Sessions\root(SSH)@192.168.70.10.xsh`
	if target := XshellSessionTargetFromPath(path); target != "192.168.70.10" {
		t.Fatalf("target = %q", target)
	}
}

func TestLatestXshellSessionTargetUsesRecentNewestFile(t *testing.T) {
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.Local)
	paths := []string{
		`C:\Sessions\root(SSH)@192.168.70.10.xsh`,
		`C:\Sessions\root(SSH)@192.168.70.11.xsh`,
	}
	mods := map[string]time.Time{
		paths[0]: now.Add(-1 * time.Minute),
		paths[1]: now.Add(-10 * time.Minute),
	}
	target := LatestXshellSessionTarget(paths, func(path string) (time.Time, error) {
		return mods[path], nil
	}, 5*time.Minute, now)
	if target != "192.168.70.10" {
		t.Fatalf("target = %q", target)
	}
}
