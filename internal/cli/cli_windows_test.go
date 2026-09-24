package cli

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("OPSERA_TEST_XSHELLCORE_HELPER") == "1" {
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestLatestDASUSMSessionsDetectsSimulatedXshellCoreProcess(t *testing.T) {
	currentExe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	fakeExe := filepath.Join(t.TempDir(), "XshellCore.exe")
	if err := copyFile(fakeExe, currentExe); err != nil {
		t.Fatalf("copy test helper: %v", err)
	}

	cmd := exec.Command(fakeExe,
		"-setviewer", "1",
		"-authprompt",
		"-url", "ssh://sim-user:sim-secret@127.0.0.1:2222",
		"-newtab", "10.0.0.9:22",
	)
	cmd.Env = append(os.Environ(), "OPSERA_TEST_XSHELLCORE_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start simulated XshellCore: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		sessions, err := LatestDASUSMSessions()
		if err == nil {
			for _, session := range sessions {
				if session.UserName == "sim-user" &&
					session.Password == "sim-secret" &&
					session.Host == "127.0.0.1" &&
					session.Port == 2222 {
					return
				}
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatal("simulated XshellCore process was not detected")
}

func copyFile(dst, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
