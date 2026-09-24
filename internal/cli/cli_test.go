package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
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

func TestParseSSOOptions(t *testing.T) {
	opts, err := parseSSOOptions([]string{"--core-only", "--window-keepalive", "--no-browser-keepalive"})
	if err != nil {
		t.Fatalf("parseSSOOptions returned error: %v", err)
	}
	if !opts.CoreOnly || !opts.WindowKeepAlive || !opts.NoBrowserKeepAlive {
		t.Fatalf("unexpected options: %#v", opts)
	}
	if _, err := parseSSOOptions([]string{"--bad"}); err == nil {
		t.Fatal("unknown option should return error")
	}
}

func TestDASUSMBrowserKeepAliveInterval(t *testing.T) {
	t.Setenv("OPSERA_DASUSM_BROWSER_KEEPALIVE_INTERVAL_MINUTES", "20")
	if got := dasusmBrowserKeepAliveInterval(); got != 20*time.Minute {
		t.Fatalf("interval = %s", got)
	}
	t.Setenv("OPSERA_DASUSM_BROWSER_KEEPALIVE_INTERVAL_MINUTES", "1")
	if got := dasusmBrowserKeepAliveInterval(); got != 3*time.Minute {
		t.Fatalf("minimum interval = %s", got)
	}
}

func TestDASUSMBrowserKeepAliveEnabled(t *testing.T) {
	if !dasusmBrowserKeepAliveEnabled(ssoOptions{}) {
		t.Fatal("browser keepalive should be enabled by default")
	}
	if dasusmBrowserKeepAliveEnabled(ssoOptions{NoBrowserKeepAlive: true}) {
		t.Fatal("--no-browser-keepalive should disable browser keepalive")
	}
	t.Setenv("OPSERA_DASUSM_BROWSER_KEEPALIVE", "0")
	if dasusmBrowserKeepAliveEnabled(ssoOptions{}) {
		t.Fatal("env should disable browser keepalive")
	}
}

func TestDASUSMPoolSetCoreOnlyClearsFailedCache(t *testing.T) {
	pool := NewDASUSMPool(nil)
	session := xsh.Session{
		Host:     "203.0.113.10",
		Port:     60022,
		UserName: "user",
		Password: "expired-token",
	}
	pool.markSessionFailed(session)
	if !pool.sessionBlocked(session) {
		t.Fatal("failed session should be blocked before mode switch")
	}
	pool.SetCoreOnly(true)
	if pool.sessionBlocked(session) {
		t.Fatal("mode switch should clear failed session cache")
	}
	if !pool.coreOnly {
		t.Fatal("coreOnly should be enabled")
	}
	pool.SetCoreOnly(false)
	if pool.coreOnly {
		t.Fatal("coreOnly should be disabled")
	}
}

func TestLiveTunnelSessionCandidateProbesTunnelPorts(t *testing.T) {
	previous := probeTunnelSession
	defer func() { probeTunnelSession = previous }()

	probed := []int{}
	probeTunnelSession = func(session xsh.Session) error {
		probed = append(probed, session.Port)
		if session.Port == 59604 {
			return nil
		}
		return fmt.Errorf("reset")
	}

	base := xsh.Session{
		Host:     "127.0.0.1",
		Port:     49442,
		UserName: "root",
		Password: "token",
	}
	session, failures, ok := liveTunnelSessionCandidate(base, []int{49414, 49442, 59604})
	if !ok {
		t.Fatalf("expected live tunnel candidate, failures=%v", failures)
	}
	if session.Port != 59604 || session.Host != "127.0.0.1" || session.UserName != "root" {
		t.Fatalf("unexpected session: %#v", session)
	}
	if len(probed) != 2 || probed[0] != 49414 || probed[1] != 59604 {
		t.Fatalf("probed ports = %v", probed)
	}
	if len(failures) != 1 || !strings.Contains(failures[0], "49414") {
		t.Fatalf("failures = %v", failures)
	}
}

func TestAttachDiagnosisDistinguishesXshellFromOpsera(t *testing.T) {
	msg := attachDiagnosis(map[string]any{
		"xshellWindow":     true,
		"xshellUrlPresent": true,
	}, errors.New("auth failed"))
	if !strings.Contains(msg, "Xshell window is online") || !strings.Contains(msg, "could not authenticate") {
		t.Fatalf("unexpected diagnosis: %s", msg)
	}
}

func TestValidateUploadLargeAgentResultRejectsIncompleteResult(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "upload-large-*.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("test payload"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := validateUploadLargeAgentResult(file.Name(), uploadResult{}); err == nil {
		t.Fatal("empty agent result should be rejected")
	}
}

func TestValidateUploadLargeAgentResultAcceptsCompleteResult(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "upload-large-*.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("test payload"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	sha, err := fileSHA256(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	if err := validateUploadLargeAgentResult(file.Name(), uploadResult{Size: 12, SHA256: sha}); err != nil {
		t.Fatalf("complete agent result should be accepted: %v", err)
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
	variants := sessionPasswordVariants(session)
	if len(variants) != 2 || variants[0] != "p/ss@word" || variants[1] != "p%2Fss%40word" {
		t.Fatalf("password variants = %#v", variants)
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

	sessions := sessionsFromXshellProcesses(processes, false)
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

	coreOnly := sessionsFromXshellProcesses(processes, true)
	if len(coreOnly) != 2 {
		t.Fatalf("len(coreOnly) = %d", len(coreOnly))
	}
	if coreOnly[0].Password != "new-core-token" || coreOnly[1].Password != "old-core-token" {
		t.Fatalf("core-only sessions should contain only XshellCore URLs in newest-first order")
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

func TestDASUSMPoolClearFailuresUnblocksSession(t *testing.T) {
	pool := NewDASUSMPool(nil)
	session := xsh.Session{
		Host:     "203.0.113.10",
		Port:     60022,
		UserName: "user",
		Password: "token",
	}
	pool.markSessionFailed(session)
	if !pool.sessionBlocked(session) {
		t.Fatal("failed session should be blocked before clear")
	}
	pool.ClearFailures()
	if pool.sessionBlocked(session) {
		t.Fatal("ClearFailures should unblock the session")
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
