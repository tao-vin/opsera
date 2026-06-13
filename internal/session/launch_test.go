package session

import "testing"

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

func TestLaunchTargetIgnoresCliHelpAndVersion(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"version"}, {"--help"}, {"--version"}} {
		if target := LaunchTarget(args); target != "" {
			t.Fatalf("LaunchTarget(%v) = %q", args, target)
		}
	}
}
