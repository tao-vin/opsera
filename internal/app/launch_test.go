package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tao-vin/opsera/internal/config"
	"github.com/tao-vin/opsera/internal/crypto"
	"github.com/tao-vin/opsera/internal/logs"
	"github.com/tao-vin/opsera/internal/model"
	"github.com/tao-vin/opsera/internal/session"
	"github.com/tao-vin/opsera/internal/sshpool"
)

func TestBastionLaunchAddsServerAndOpensTerminal(t *testing.T) {
	rt := newTestRuntime(t)

	rt.handleLaunchArgs([]string{"opsera://192.168.70.10"})

	state := rt.store.Snapshot()
	var server model.Server
	for _, item := range state.Servers {
		if item.Host == "192.168.70.10" {
			server = item
			break
		}
	}
	if server.ID == "" {
		t.Fatal("launch target was not added to server list")
	}
	if server.Mode != model.ConnectionModeVPNLauncher {
		t.Fatalf("server mode = %q", server.Mode)
	}
	sessions := rt.sessions.List()
	if len(sessions) == 0 || sessions[0].Target != "192.168.70.10" {
		t.Fatalf("sessions = %#v", sessions)
	}

	ui := newUIState(rt, nil)
	if ui.selectedServer < 0 || ui.selectedServer >= len(ui.servers) {
		t.Fatalf("selectedServer = %d", ui.selectedServer)
	}
	if ui.servers[ui.selectedServer].Host != "192.168.70.10" {
		t.Fatalf("selected host = %q", ui.servers[ui.selectedServer].Host)
	}
	if len(ui.terminalTabs) == 0 {
		t.Fatal("terminal tab was not opened")
	}
	if ui.terminalTabs[ui.activeTerminal].Target != "192.168.70.10" {
		t.Fatalf("active tab target = %q", ui.terminalTabs[ui.activeTerminal].Target)
	}
}

func TestBastionLaunchDoesNotDuplicateServerSessionOrTab(t *testing.T) {
	rt := newTestRuntime(t)

	rt.handleLaunchArgs([]string{"opsera://192.168.70.10"})
	rt.handleLaunchArgs([]string{"--host", "192.168.70.10"})

	state := rt.store.Snapshot()
	count := 0
	for _, item := range state.Servers {
		if item.Host == "192.168.70.10" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("server count for target = %d", count)
	}
	sessions := rt.sessions.List()
	sessionCount := 0
	for _, item := range sessions {
		if item.Target == "192.168.70.10" {
			sessionCount++
		}
	}
	if sessionCount != 1 {
		t.Fatalf("session count for target = %d; sessions=%#v", sessionCount, sessions)
	}

	ui := newUIState(rt, nil)
	ui.openLatestSessionTab()
	tabCount := 0
	for _, tab := range ui.terminalTabs {
		if tab.Target == "192.168.70.10" {
			tabCount++
		}
	}
	if tabCount != 1 {
		t.Fatalf("tab count for target = %d; tabs=%#v", tabCount, ui.terminalTabs)
	}
}

func TestForwardedBastionLaunchRefreshesExistingUI(t *testing.T) {
	rt := newTestRuntime(t)
	ui := newUIState(rt, nil)
	if len(ui.terminalTabs) != 0 {
		t.Fatalf("unexpected initial terminal tabs: %#v", ui.terminalTabs)
	}

	rt.sessions.Start("192.168.70.10", "forwarded")
	rt.handleForwardedLaunch([]string{"--host", "192.168.70.10"})
	ui.pollLaunchChanges()

	if ui.selectedServer < 0 || ui.servers[ui.selectedServer].Host != "192.168.70.10" {
		t.Fatalf("selectedServer=%d servers=%#v", ui.selectedServer, ui.servers)
	}
	if len(ui.terminalTabs) != 1 || ui.terminalTabs[0].Target != "192.168.70.10" {
		t.Fatalf("terminalTabs=%#v", ui.terminalTabs)
	}
}

func TestBastionLaunchInfersTargetFromRecentXshellSessionFile(t *testing.T) {
	rt := newTestRuntime(t)
	appData := filepath.Join(t.TempDir(), "Roaming")
	t.Setenv("APPDATA", appData)
	sessionDir := filepath.Join(appData, "NetSarang", "Xshell", "Sessions")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	xshPath := filepath.Join(sessionDir, "root(SSH)@192.168.70.10.xsh")
	if err := os.WriteFile(xshPath, []byte("[CONNECTION]\nHost=127.0.0.1\nPort=61102\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(xshPath, now, now); err != nil {
		t.Fatal(err)
	}

	rt.handleLaunchArgs(nil)

	state := rt.store.Snapshot()
	found := false
	for _, server := range state.Servers {
		if server.Host == "192.168.70.10" && server.Mode == model.ConnectionModeVPNLauncher {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("inferred target was not added to servers: %#v", state.Servers)
	}
	sessions := rt.sessions.List()
	if len(sessions) == 0 || sessions[0].Target != "192.168.70.10" {
		t.Fatalf("sessions=%#v", sessions)
	}
}

func newTestRuntime(t *testing.T) *runtime {
	t.Helper()
	root := t.TempDir()
	store, err := config.NewStore(filepath.Join(root, "config"))
	if err != nil {
		t.Fatal(err)
	}
	logStore, err := logs.NewStore(filepath.Join(root, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	vault := crypto.NewVault(root)
	return &runtime{
		store:    store,
		logStore: logStore,
		vault:    vault,
		dataDir:  root,
		sessions: session.NewManager(store, logStore),
		commands: session.NewCommandQueue(logStore),
		sshPool:  sshpool.New(store, vault, logStore),
	}
}
