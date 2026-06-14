package cli

import (
	"testing"
	"time"

	"github.com/tao-vin/opsera/internal/xsh"
)

func TestTryRunHandlesHelpAndVersion(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"--help"}, {"version"}, {"--version"}} {
		handled, err := TryRun(args)
		if err != nil {
			t.Fatalf("TryRun(%v) returned error: %v", args, err)
		}
		if !handled {
			t.Fatalf("TryRun(%v) was not handled", args)
		}
	}
}

func TestSessionFromSSHURLInTextParsesXshellCoreCommandLine(t *testing.T) {
	line := `"C:\Program Files (x86)\NetSarang\Xshell 7\XshellCore.exe" -setviewer 395718 -authprompt -url "ssh://sso-user:p%2Fss%40word@203.0.113.10:60022" -newtab "10.0.0.9:22"`

	session, err := sessionFromSSHURLInText(line)
	if err != nil {
		t.Fatalf("sessionFromSSHURLInText returned error: %v", err)
	}
	if session.UserName != "sso-user" {
		t.Fatalf("username = %q", session.UserName)
	}
	if session.Password != "p/ss@word" {
		t.Fatalf("password was not URL-decoded")
	}
	if session.Host != "203.0.113.10" {
		t.Fatalf("host = %q", session.Host)
	}
	if session.Port != 60022 {
		t.Fatalf("port = %d", session.Port)
	}
}

func TestSessionsFromXshellProcessesPrefersNewestCoreURL(t *testing.T) {
	processes := []xshellProcessSnapshot{
		{
			ProcessID:   20,
			Name:        "XshellCore.exe",
			CommandLine: `"C:\XshellCore.exe" -url "ssh://user:old-core-token@203.0.113.10:60022" -newtab "10.0.0.9:22"`,
		},
		{
			ProcessID:   30,
			Name:        "XshellCore.exe",
			CommandLine: `"C:\XshellCore.exe" -url "ssh://user:new-core-token@203.0.113.10:60022" -newtab "10.0.0.9:22"`,
		},
		{
			ProcessID:   10,
			Name:        "Xshell.exe",
			CommandLine: `"C:\Xshell.exe" -url ssh://user:launcher-token@203.0.113.10:60022 -newtab 10.0.0.9:22`,
		},
	}

	sessions := sessionsFromXshellProcesses(processes)
	if len(sessions) != 3 {
		t.Fatalf("len(sessions) = %d", len(sessions))
	}
	if sessions[0].Password != "new-core-token" {
		t.Fatalf("first session should come from newest XshellCore.exe URL")
	}
	if sessions[1].Password != "old-core-token" {
		t.Fatalf("second session should come from older XshellCore.exe URL")
	}
	if sessions[2].Password != "launcher-token" {
		t.Fatalf("launcher URL should be fallback after XshellCore.exe URLs")
	}
}

func TestDASUSMPoolBlocksFailedSessionTemporarily(t *testing.T) {
	pool := NewDASUSMPool(nil)
	session := xsh.Session{
		Host:     "203.0.113.10",
		Port:     60022,
		UserName: "user",
		Password: "expired-token",
	}

	if pool.sessionBlocked(session) {
		t.Fatal("fresh session should not be blocked")
	}
	pool.markSessionFailed(session)
	if !pool.sessionBlocked(session) {
		t.Fatal("failed session should be blocked")
	}
	pool.failed[sessionCacheKey(session)] = time.Now().Add(-11 * time.Minute)
	if pool.sessionBlocked(session) {
		t.Fatal("expired block should be cleared")
	}
}

func TestLatestXshellCoreSessionFromLinesUsesSimulatedCoreCommandLine(t *testing.T) {
	session, err := latestXshellCoreSessionFromLines([]string{
		`"C:\XshellCore.exe" -setviewer 1 -authprompt -url "ssh://agent:secret@127.0.0.1:2222" -newtab "10.0.0.9:22"`,
	})
	if err != nil {
		t.Fatalf("latestXshellCoreSessionFromLines returned error: %v", err)
	}
	if session.UserName != "agent" || session.Password != "secret" || session.Host != "127.0.0.1" || session.Port != 2222 {
		t.Fatalf("unexpected session: %#v", session)
	}
}
