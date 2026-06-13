package cli

import "testing"

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

func TestSessionsFromXshellProcessesPrefersLauncherURL(t *testing.T) {
	processes := []xshellProcessSnapshot{
		{
			ProcessID:   20,
			Name:        "XshellCore.exe",
			CommandLine: `"C:\XshellCore.exe" -url "ssh://user:core-token@203.0.113.10:60022" -newtab "10.0.0.9:22"`,
		},
		{
			ProcessID:   10,
			Name:        "Xshell.exe",
			CommandLine: `"C:\Xshell.exe" -url ssh://user:launcher-token@203.0.113.10:60022 -newtab 10.0.0.9:22`,
		},
	}

	sessions := sessionsFromXshellProcesses(processes)
	if len(sessions) != 2 {
		t.Fatalf("len(sessions) = %d", len(sessions))
	}
	if sessions[0].Password != "launcher-token" {
		t.Fatalf("first session should come from Xshell.exe launcher URL")
	}
	if sessions[1].Password != "core-token" {
		t.Fatalf("second session should come from XshellCore.exe URL")
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
