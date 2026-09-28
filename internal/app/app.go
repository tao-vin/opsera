package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"gioui.org/app"
	"gioui.org/font"
	"gioui.org/gesture"
	"gioui.org/io/clipboard"
	"gioui.org/io/key"
	"gioui.org/io/system"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/tao-vin/opsera/internal/api"
	opcli "github.com/tao-vin/opsera/internal/cli"
	"github.com/tao-vin/opsera/internal/config"
	"github.com/tao-vin/opsera/internal/crypto"
	"github.com/tao-vin/opsera/internal/events"
	"github.com/tao-vin/opsera/internal/logs"
	"github.com/tao-vin/opsera/internal/model"
	"github.com/tao-vin/opsera/internal/session"
	"github.com/tao-vin/opsera/internal/sshpool"
	"github.com/tao-vin/opsera/internal/version"
)

type runtime struct {
	store      *config.Store
	logStore   *logs.Store
	vault      *crypto.Vault
	sessions   *session.Manager
	commands   *session.CommandQueue
	sshPool    *sshpool.Pool
	httpServer *http.Server
	dataDir    string
	identity   localIdentity
	relayHost  string
	keepersMu  sync.Mutex
	keepers    []io.Closer
	launchMu   sync.Mutex
	launchSeq  int64
}

type localIdentity struct {
	MachineCode string `json:"machineCode"`
	Password    string `json:"password"`
	Secret      string `json:"secret"`
}

type relayConfig struct {
	Host string `json:"host"`
}

type uiState struct {
	rt     *runtime
	window *app.Window
	th     *material.Theme

	servers            []model.Server
	credentials        []model.Credential
	logItems           []model.LogEntry
	commandItems       []model.Command
	terminalTabs       []terminalTab
	activeTerminal     int
	focusTerminalInput bool
	seenEvents         map[string]bool
	startedAt          time.Time
	lastEventPoll      time.Time
	lastConfigPoll     time.Time
	lastLaunchSeq      int64
	commandResults     chan terminalCommandResult
	maximized          bool

	selectedServer int
	editing        bool
	tab            string
	status         string
	confirmDelete  bool
	pendingDelete  int

	name        widget.Editor
	address     widget.Editor
	port        widget.Editor
	username    widget.Editor
	password    widget.Editor
	notes       widget.Editor
	placeholder widget.Editor
	machineType model.MachineType

	addBtn            widget.Clickable
	saveBtn           widget.Clickable
	linuxModeBtn      widget.Clickable
	windowsModeBtn    widget.Clickable
	serversTab        widget.Clickable
	logsTab           widget.Clickable
	terminalTabClicks []widget.Clickable
	closeTabClicks    []widget.Clickable
	serverClicks      []widget.Clickable
	rowEditClicks     []widget.Clickable
	rowDeleteClicks   []widget.Clickable
	confirmDeleteBtn  widget.Clickable
	cancelDeleteBtn   widget.Clickable
	copyRelayBtn      widget.Clickable
	minimizeBtn       widget.Clickable
	maximizeBtn       widget.Clickable
	closeBtn          widget.Clickable
	headerDrag        gesture.Click
}

type terminalTab struct {
	ID           string
	Title        string
	Target       string
	ServerID     string
	CommandInput widget.Editor
	HistoryList  widget.List
	Commands     []model.Command
	Lines        []terminalLine
	History      []string
	HistoryIndex int
}

type terminalCommandResult struct {
	tabID     string
	commandID string
	output    string
	err       error
}

func Run() error {
	if forwardLaunch(os.Args[1:]) {
		return nil
	}
	dataDir, err := resolveDataDir()
	if err != nil {
		return err
	}
	store, err := config.NewStore(filepath.Join(dataDir, "config"))
	if err != nil {
		return err
	}
	logStore, err := logs.NewStore(filepath.Join(dataDir, "logs"))
	if err != nil {
		return err
	}
	identity, err := loadLocalIdentity(dataDir)
	if err != nil {
		return err
	}
	relayConfig, err := loadRelayConfig(dataDir)
	if err != nil {
		return err
	}
	rt := &runtime{
		store:     store,
		logStore:  logStore,
		vault:     crypto.NewVault(dataDir),
		dataDir:   dataDir,
		identity:  identity,
		relayHost: relayConfig.Host,
	}
	rt.sessions = session.NewManager(store, logStore)
	rt.commands = session.NewCommandQueue(logStore)
	rt.sshPool = sshpool.New(store, rt.vault, logStore)
	if err := rt.startAPI(); err != nil {
		return err
	}
	_ = ensureTrayIconFile(dataDir)
	stopTray := startTrayIcon(dataDir)
	defer stopTray()
	rt.startRelayPresence()
	rt.startRelayAgent()
	rt.handleLaunchArgs(os.Args[1:])

	errCh := make(chan error, 1)
	go func() {
		window := new(app.Window)
		window.Option(app.Title("Opsera"))
		window.Option(app.Decorated(false))
		window.Option(app.Maximized.Option())
		errCh <- rt.runWindow(window)
		os.Exit(0)
	}()
	app.Main()

	if rt.httpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = rt.httpServer.Shutdown(ctx)
	}
	if rt.sshPool != nil {
		rt.sshPool.CloseAll()
	}
	select {
	case err := <-errCh:
		return err
	default:
		return nil
	}
}

func (r *runtime) startAPI() error {
	server := api.New(r.store, r.logStore, r.vault, r.sessions, r.commands, r.sshPool, r.handleForwardedLaunch, r.runOpenSessionCommand, activateExistingWindow)
	r.httpServer = &http.Server{
		Addr:    "127.0.0.1:18741",
		Handler: server.Handler(),
	}
	go func() {
		err := r.httpServer.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			_ = r.logStore.Append(model.LogLevelError, "api", "listen failed: "+err.Error(), "")
		}
	}()
	return nil
}

func (r *runtime) startRelayPresence() {
	if strings.TrimSpace(r.relayHost) == "" || strings.TrimSpace(r.identity.Secret) == "" {
		return
	}
	go func() {
		if err := r.registerRelayDevice(); err != nil {
			_ = r.logStore.Append(model.LogLevelError, "relay", "register failed: "+err.Error(), "")
		} else {
			_ = r.logStore.Append(model.LogLevelInfo, "relay", "registered: "+r.identity.MachineCode, "")
		}
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if err := r.heartbeatRelayDevice(); err != nil {
				_ = r.logStore.Append(model.LogLevelWarn, "relay", "heartbeat failed: "+err.Error(), "")
			}
		}
	}()
}

func (r *runtime) registerRelayDevice() error {
	body := map[string]string{
		"machineCode": r.identity.MachineCode,
		"name":        os.Getenv("COMPUTERNAME"),
		"secret":      r.identity.Secret,
		"accessCode":  r.identity.Password,
		"version":     version.Version,
		"buildTime":   version.BuildTime,
		"commit":      version.Commit,
	}
	if strings.TrimSpace(body["name"]) == "" {
		body["name"] = "Opsera Windows"
	}
	return r.postRelayJSON("/devices/register", body)
}

func (r *runtime) heartbeatRelayDevice() error {
	return r.postRelayJSON("/devices/heartbeat", map[string]string{
		"machineCode": r.identity.MachineCode,
		"secret":      r.identity.Secret,
		"version":     version.Version,
		"buildTime":   version.BuildTime,
		"commit":      version.Commit,
	})
}

func (r *runtime) postRelayJSON(path string, body map[string]string) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	url := relayURL(r.relayHost, path)
	client := http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	return nil
}

func relayURL(host, path string) string {
	host = strings.TrimSpace(host)
	if strings.HasPrefix(host, "http://") || strings.HasPrefix(host, "https://") {
		return strings.TrimRight(host, "/") + path
	}
	return "https://" + strings.TrimRight(host, "/") + path
}

func (r *runtime) handleLaunchArgs(args []string) {
	if len(args) > 0 {
		raw, err := json.Marshal(args)
		if err == nil {
			_ = r.logStore.Append(model.LogLevelInfo, "argv", string(raw), "")
		} else {
			_ = r.logStore.Append(model.LogLevelInfo, "argv", strings.Join(args, " "), "")
		}
	}
	target := session.LaunchTarget(args)
	if target == "" {
		target = latestXshellSessionTarget(5 * time.Minute)
		if target != "" {
			_ = r.logStore.Append(model.LogLevelInfo, "argv", "launch target inferred from latest xshell session: "+target, "")
		}
	}
	if target == "" {
		_ = r.logStore.Append(model.LogLevelWarn, "argv", "launch target not found", strings.Join(args, " "))
		return
	}
	r.ensureLaunchServer(target)
	r.sessions.Start(target, "argv")
	r.markLaunchChanged()
	r.holdLaunchTunnel(args)
}

func (r *runtime) handleForwardedLaunch(args []string) {
	target := session.LaunchTarget(args)
	if target == "" {
		target = latestXshellSessionTarget(5 * time.Minute)
		if target != "" {
			_ = r.logStore.Append(model.LogLevelInfo, "argv", "forwarded launch target inferred from latest xshell session: "+target, "")
		}
	}
	if target == "" {
		_ = r.logStore.Append(model.LogLevelWarn, "argv", "forwarded launch target not found", strings.Join(args, " "))
		return
	}
	r.ensureLaunchServer(target)
	r.markLaunchChanged()
	r.holdLaunchTunnel(args)
}

func (r *runtime) markLaunchChanged() {
	r.launchMu.Lock()
	r.launchSeq++
	r.launchMu.Unlock()
}

func (r *runtime) launchVersion() int64 {
	r.launchMu.Lock()
	defer r.launchMu.Unlock()
	return r.launchSeq
}

func (r *runtime) runOpenSessionCommand(target string, command string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		items := r.sessions.List()
		if len(items) > 0 {
			target = strings.TrimSpace(items[0].Target)
		}
	}
	if server, ok := r.directServerForTarget(target); ok {
		return r.sshPool.Run(server.ID, command)
	}
	return opcli.RunCommand("", command)
}

func (r *runtime) directServerForTarget(target string) (model.Server, bool) {
	q := strings.ToLower(strings.TrimSpace(target))
	if q == "" {
		return model.Server{}, false
	}
	for _, server := range r.store.Snapshot().Servers {
		if !isDirectSSHServer(server) {
			continue
		}
		if strings.ToLower(server.ID) == q ||
			strings.ToLower(server.Name) == q ||
			strings.ToLower(server.Host) == q ||
			strings.Contains(strings.ToLower(server.Name), q) {
			return server, true
		}
	}
	return model.Server{}, false
}

func (r *runtime) ensureLaunchServer(target string) {
	target = strings.TrimSpace(target)
	if target == "" {
		return
	}
	state := r.store.Snapshot()
	for _, server := range state.Servers {
		if strings.EqualFold(server.Host, target) {
			return
		}
	}
	id := "vpn-" + target
	_ = r.store.UpsertServer(model.Server{
		ID:   id,
		Name: target,
		Host: target,
		Mode: model.ConnectionModeVPNLauncher,
	})
	_ = r.logStore.Append(model.LogLevelInfo, "ui", "bastion opsera server added: "+target, id)
}

func (r *runtime) holdLaunchTunnel(args []string) {
	xshPath := launchXshPath(args)
	if xshPath == "" {
		return
	}
	keeper, err := opcli.OpenKeepAlive(xshPath)
	if err != nil {
		_ = r.logStore.Append(model.LogLevelError, "tunnel", "keepalive failed: "+err.Error(), "")
		return
	}
	r.keepersMu.Lock()
	r.keepers = append(r.keepers, keeper)
	if len(r.keepers) > 8 {
		old := r.keepers[0]
		r.keepers = r.keepers[1:]
		_ = old.Close()
	}
	r.keepersMu.Unlock()
	_ = r.logStore.Append(model.LogLevelInfo, "tunnel", "keepalive started: "+xshPath, "")
}

func launchXshPath(args []string) string {
	for _, arg := range args {
		value := strings.Trim(strings.TrimSpace(arg), `"`)
		if strings.HasSuffix(strings.ToLower(value), ".xsh") {
			return value
		}
	}
	return ""
}

func latestXshellSessionTarget(maxAge time.Duration) string {
	pattern := filepath.Join(os.Getenv("APPDATA"), "NetSarang", "Xshell", "Sessions", "*.xsh")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return ""
	}
	return session.LatestXshellSessionTarget(matches, func(path string) (time.Time, error) {
		info, err := os.Stat(path)
		if err != nil {
			return time.Time{}, err
		}
		return info.ModTime(), nil
	}, maxAge, time.Now())
}

func forwardLaunch(args []string) bool {
	raw, err := json.Marshal(map[string][]string{"args": args})
	if err != nil {
		return false
	}
	client := http.Client{Timeout: 800 * time.Millisecond}
	if len(args) == 0 {
		resp, err := client.Post("http://127.0.0.1:18741/activate", "application/json", bytes.NewReader([]byte("{}")))
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode >= 200 && resp.StatusCode < 300
	}
	resp, err := client.Post("http://127.0.0.1:18741/launch", "application/json", bytes.NewReader(raw))
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func (r *runtime) runWindow(window *app.Window) error {
	ui := newUIState(r, window)
	var ops op.Ops
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				window.Invalidate()
			case <-done:
				return
			}
		}
	}()
	defer close(done)
	for {
		switch e := window.Event().(type) {
		case app.DestroyEvent:
			return e.Err
		case app.ConfigEvent:
			ui.maximized = e.Config.Mode == app.Maximized
		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)
			ui.handle(gtx)
			ui.layout(gtx)
			e.Frame(gtx.Ops)
		}
	}
}

func newUIState(rt *runtime, window *app.Window) *uiState {
	ui := &uiState{
		rt:             rt,
		window:         window,
		th:             material.NewTheme(),
		selectedServer: -1,
		editing:        false,
		tab:            "servers",
		status:         "Ready",
		machineType:    model.MachineTypeLinux,
		seenEvents:     map[string]bool{},
		startedAt:      time.Now(),
		lastLaunchSeq:  rt.launchVersion(),
		commandResults: make(chan terminalCommandResult, 32),
		maximized:      true,
	}
	ui.name.SingleLine = true
	ui.address.SingleLine = true
	ui.port.SingleLine = true
	ui.username.SingleLine = true
	ui.password.SingleLine = true
	ui.notes.SingleLine = false
	ui.placeholder.SingleLine = true
	ui.placeholder.ReadOnly = true
	_ = events.Prune(rt.dataDir, 200)
	ui.reload()
	ui.openLatestSessionTab()
	return ui
}

func (ui *uiState) addTerminalTab(title string, target string) {
	ui.addTerminalTabForServer(title, target, "")
}

func (ui *uiState) addTerminalTabForServer(title string, target string, serverID string) {
	for i := range ui.terminalTabs {
		if serverID != "" && ui.terminalTabs[i].ServerID == serverID {
			ui.activeTerminal = i
			return
		}
		if serverID == "" && ui.terminalTabs[i].Target == target && target != "" {
			ui.activeTerminal = i
			return
		}
	}
	tab := terminalTab{
		ID:       fmt.Sprintf("tab-%d", time.Now().UnixNano()),
		Title:    title,
		Target:   target,
		ServerID: serverID,
	}
	tab.CommandInput.SingleLine = true
	tab.CommandInput.Submit = true
	tab.HistoryList.Axis = layout.Vertical
	tab.HistoryList.List.ScrollToEnd = true
	tab.HistoryIndex = -1
	ui.terminalTabs = append(ui.terminalTabs, tab)
	ui.terminalTabClicks = append(ui.terminalTabClicks, widget.Clickable{})
	ui.closeTabClicks = append(ui.closeTabClicks, widget.Clickable{})
	ui.activeTerminal = len(ui.terminalTabs) - 1
}

func (ui *uiState) reload() {
	state := ui.rt.store.Snapshot()
	ui.servers = state.Servers
	ui.credentials = state.Credentials
	logItems, err := ui.rt.logStore.ReadLatest(80)
	if err != nil {
		ui.status = err.Error()
		return
	}
	ui.logItems = logItems
	for len(ui.serverClicks) < len(ui.servers) {
		ui.serverClicks = append(ui.serverClicks, widget.Clickable{})
	}
	for len(ui.rowEditClicks) < len(ui.servers) {
		ui.rowEditClicks = append(ui.rowEditClicks, widget.Clickable{})
	}
	for len(ui.rowDeleteClicks) < len(ui.servers) {
		ui.rowDeleteClicks = append(ui.rowDeleteClicks, widget.Clickable{})
	}
}

func (ui *uiState) handle(gtx layout.Context) {
	ui.drainCommandResults()
	ui.pollLaunchChanges()
	ui.pollEvents()
	ui.pollConfig()
	for ui.minimizeBtn.Clicked(gtx) {
		ui.window.Perform(system.ActionMinimize)
	}
	for ui.maximizeBtn.Clicked(gtx) {
		if ui.maximized {
			ui.window.Perform(system.ActionUnmaximize)
			ui.maximized = false
		} else {
			ui.window.Perform(system.ActionMaximize)
			ui.maximized = true
		}
	}
	for ui.closeBtn.Clicked(gtx) {
		ui.window.Perform(system.ActionClose)
	}
	for ui.addBtn.Clicked(gtx) {
		ui.clearForm()
		ui.editing = true
	}
	for ui.serversTab.Clicked(gtx) {
		ui.tab = "servers"
	}
	for ui.logsTab.Clicked(gtx) {
		ui.tab = "logs"
		ui.reload()
	}
	for i := range ui.terminalTabs {
		for ui.terminalTabClicks[i].Clicked(gtx) {
			ui.activeTerminal = i
			ui.selectServerByTarget(ui.terminalTabs[i].Target)
		}
		for ui.closeTabClicks[i].Clicked(gtx) {
			ui.terminalTabs = append(ui.terminalTabs[:i], ui.terminalTabs[i+1:]...)
			ui.terminalTabClicks = append(ui.terminalTabClicks[:i], ui.terminalTabClicks[i+1:]...)
			ui.closeTabClicks = append(ui.closeTabClicks[:i], ui.closeTabClicks[i+1:]...)
			if ui.activeTerminal >= len(ui.terminalTabs) {
				ui.activeTerminal = len(ui.terminalTabs) - 1
			}
			if ui.activeTerminal >= 0 && ui.activeTerminal < len(ui.terminalTabs) {
				ui.selectServerByTarget(ui.terminalTabs[ui.activeTerminal].Target)
			} else {
				ui.selectedServer = -1
			}
		}
		for {
			ev, ok := ui.terminalTabs[i].CommandInput.Update(gtx)
			if !ok {
				break
			}
			if submit, ok := ev.(widget.SubmitEvent); ok {
				ui.activeTerminal = i
				text := ui.terminalTabs[i].CommandInput.Text()
				if strings.TrimSpace(text) == "" {
					text = submit.Text
				}
				ui.runCommand(text)
			}
		}
		for {
			ev, ok := gtx.Event(key.Filter{
				Focus: &ui.terminalTabs[i].CommandInput,
				Name:  key.NameUpArrow,
			})
			if !ok {
				break
			}
			if keyEvent, ok := ev.(key.Event); ok && keyEvent.State == key.Press {
				ui.activeTerminal = i
				ui.navigateCommandHistory(i, true)
			}
		}
		for {
			ev, ok := gtx.Event(key.Filter{
				Focus: &ui.terminalTabs[i].CommandInput,
				Name:  key.NameDownArrow,
			})
			if !ok {
				break
			}
			if keyEvent, ok := ev.(key.Event); ok && keyEvent.State == key.Press {
				ui.activeTerminal = i
				ui.navigateCommandHistory(i, false)
			}
		}
		for {
			ev, ok := gtx.Event(key.Filter{
				Focus: &ui.terminalTabs[i].CommandInput,
				Name:  key.NameTab,
			})
			if !ok {
				break
			}
			if keyEvent, ok := ev.(key.Event); ok && keyEvent.State == key.Press {
				ui.activeTerminal = i
				ui.completeCommandInput(i)
			}
		}
	}
	for ui.saveBtn.Clicked(gtx) {
		ui.save()
	}
	for ui.linuxModeBtn.Clicked(gtx) {
		ui.machineType = model.MachineTypeLinux
		ui.status = "Linux SSH mode"
	}
	for ui.windowsModeBtn.Clicked(gtx) {
		ui.machineType = model.MachineTypeWindows
		ui.status = "Windows relay mode"
	}
	for ui.confirmDeleteBtn.Clicked(gtx) {
		ui.selectedServer = ui.pendingDelete
		ui.confirmDelete = false
		ui.delete()
	}
	for ui.cancelDeleteBtn.Clicked(gtx) {
		ui.confirmDelete = false
		ui.pendingDelete = -1
		ui.status = "Ready"
	}
	for i := range ui.servers {
		for ui.serverClicks[i].Clicked(gtx) {
			ui.selectedServer = i
			ui.editing = false
			ui.openServerTab(ui.servers[i])
		}
		for ui.rowEditClicks[i].Clicked(gtx) {
			ui.selectedServer = i
			ui.load(ui.servers[i])
			ui.editing = true
		}
		for ui.rowDeleteClicks[i].Clicked(gtx) {
			ui.pendingDelete = i
			ui.confirmDelete = true
			ui.status = "Confirm delete"
		}
	}
}

func (ui *uiState) pollLaunchChanges() {
	seq := ui.rt.launchVersion()
	if seq == ui.lastLaunchSeq {
		return
	}
	ui.lastLaunchSeq = seq
	ui.reload()
	ui.openLatestSessionTab()
	ui.status = "Opened bastion session"
}

func (ui *uiState) drainCommandResults() {
	for {
		select {
		case result := <-ui.commandResults:
			ui.applyCommandResult(result)
		default:
			return
		}
	}
}

func (ui *uiState) applyCommandResult(result terminalCommandResult) {
	for tabIndex := range ui.terminalTabs {
		if ui.terminalTabs[tabIndex].ID != result.tabID {
			continue
		}
		for commandIndex := range ui.terminalTabs[tabIndex].Commands {
			item := &ui.terminalTabs[tabIndex].Commands[commandIndex]
			if item.ID != result.commandID {
				continue
			}
			item.Output = result.output
			item.UpdatedAt = time.Now().Format(time.RFC3339)
			if result.err != nil {
				item.Status = model.CommandStatusFailed
				item.Error = result.err.Error()
				ui.status = result.err.Error()
			} else {
				item.Status = model.CommandStatusDone
				item.Error = ""
				ui.status = "Command done"
			}
			ui.terminalTabs[tabIndex].Lines = terminalLines(ui.terminalTabs[tabIndex].Commands)
			ui.terminalTabs[tabIndex].HistoryList.List.Position.BeforeEnd = false
			return
		}
	}
}

func (ui *uiState) pollConfig() {
	if ui.editing || time.Since(ui.lastConfigPoll) < 5*time.Second {
		return
	}
	ui.lastConfigPoll = time.Now()
	ui.reload()
	ui.openLatestSessionTab()
}

func (ui *uiState) openLatestSessionTab() {
	items := ui.rt.sessions.List()
	if len(items) == 0 {
		return
	}
	target := strings.TrimSpace(items[0].Target)
	if target == "" {
		return
	}
	for i := range ui.terminalTabs {
		if ui.terminalTabs[i].Target == target {
			ui.activeTerminal = i
			ui.selectServerByTarget(target)
			return
		}
	}
	for _, server := range ui.servers {
		if strings.EqualFold(server.Host, target) {
			ui.openServerTab(server)
			return
		}
	}
}

func (ui *uiState) pollEvents() {
	if time.Since(ui.lastEventPoll) < 2*time.Second {
		return
	}
	ui.lastEventPoll = time.Now()
	items, err := events.ReadLatest(ui.rt.dataDir, 200)
	if err != nil {
		ui.status = err.Error()
		return
	}
	for _, event := range items {
		if ui.seenEvents[event.ID] {
			continue
		}
		if event.CreatedAt != "" {
			createdAt, err := time.Parse(time.RFC3339, event.CreatedAt)
			if err == nil && createdAt.Before(ui.startedAt) {
				ui.seenEvents[event.ID] = true
				continue
			}
		}
		ui.seenEvents[event.ID] = true
		ui.applyEvent(event)
		_ = events.Remove(ui.rt.dataDir, event)
	}
}

func (ui *uiState) applyEvent(event events.Event) {
	if len(ui.terminalTabs) == 0 {
		ui.openLatestSessionTab()
		if len(ui.terminalTabs) == 0 {
			ui.status = "Event received without active tab"
			return
		}
	}
	idx := ui.activeTerminal
	if event.TabID != "" {
		for i := range ui.terminalTabs {
			if ui.terminalTabs[i].ID == event.TabID {
				idx = i
				break
			}
		}
	}
	item := model.Command{
		ID:        event.ID,
		Command:   event.Command,
		Output:    event.Output,
		Error:     event.Error,
		CreatedAt: event.CreatedAt,
		UpdatedAt: event.CreatedAt,
	}
	if event.Status == string(model.CommandStatusDone) {
		item.Status = model.CommandStatusDone
	} else if event.Status == string(model.CommandStatusFailed) {
		item.Status = model.CommandStatusFailed
	} else {
		item.Status = model.CommandStatusQueued
	}
	ui.terminalTabs[idx].Commands = append(ui.terminalTabs[idx].Commands, item)
	ui.terminalTabs[idx].Lines = terminalLines(ui.terminalTabs[idx].Commands)
	ui.status = "Event: " + event.Type
}

func (ui *uiState) openServerTab(server model.Server) {
	title := server.Name
	if strings.TrimSpace(title) == "" {
		title = server.Host
	}
	target := server.Host
	if server.MachineType == model.MachineTypeWindows || server.Mode == model.ConnectionModeRelayAgent {
		target = server.MachineCode
	}
	ui.addTerminalTabForServer(title, target, server.ID)
	ui.focusTerminalInput = true
	ui.selectServerByTarget(server.Host)
}

func (ui *uiState) selectServerByTarget(target string) {
	for i := range ui.servers {
		server := ui.servers[i]
		if server.Host == target || server.MachineCode == target || server.ID == target {
			ui.selectedServer = i
			return
		}
	}
}

func (ui *uiState) layout(gtx layout.Context) layout.Dimensions {
	fill(gtx, color.NRGBA{R: 10, G: 11, B: 14, A: 255})
	dims := layout.Inset{}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(ui.header),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
					layout.Flexed(0.14, ui.serverList),
					layout.Rigid(layout.Spacer{Width: unit.Dp(14)}.Layout),
					layout.Flexed(0.86, func(gtx layout.Context) layout.Dimensions {
						if ui.editing {
							return ui.editor(gtx)
						}
						return ui.logs(gtx)
					}),
				)
			}),
		)
	})
	return dims
}

func (ui *uiState) header(gtx layout.Context) layout.Dimensions {
	for ui.copyRelayBtn.Clicked(gtx) {
		text := ui.relayShareText()
		gtx.Execute(clipboard.WriteCmd{Type: "application/text", Data: io.NopCloser(strings.NewReader(text))})
		ui.status = "Relay code copied"
	}
	return surface(gtx, color.NRGBA{R: 0, G: 0, B: 0, A: 255}, func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Top: unit.Dp(4), Bottom: unit.Dp(4), Left: unit.Dp(8), Right: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					macro := op.Record(gtx.Ops)
					dims := layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
						layout.Rigid(logoMark(ui.th)),
						layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							label := material.Body1(ui.th, "Opsera")
							label.Color = color.NRGBA{R: 246, G: 248, B: 251, A: 255}
							return label.Layout(gtx)
						}),
						layout.Rigid(layout.Spacer{Width: unit.Dp(12)}.Layout),
						layout.Flexed(1, layout.Spacer{}.Layout),
					)
					call := macro.Stop()
					stack := clip.Rect{Max: dims.Size}.Push(gtx.Ops)
					for {
						ev, ok := ui.headerDrag.Update(gtx.Source)
						if !ok {
							break
						}
						if ev.Kind == gesture.KindPress && ui.maximized {
							ui.window.Perform(system.ActionUnmaximize)
							ui.maximized = false
						}
					}
					ui.headerDrag.Add(gtx.Ops)
					system.ActionInputOp(system.ActionMove).Add(gtx.Ops)
					stack.Pop()
					call.Add(gtx.Ops)
					return dims
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					label := material.Caption(ui.th, fmt.Sprintf("Code %s  Password %s", ui.rt.identity.MachineCode, ui.rt.identity.Password))
					label.Color = color.NRGBA{R: 172, G: 179, B: 191, A: 255}
					return label.Layout(gtx)
				}),
				layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					btn := material.Button(ui.th, &ui.copyRelayBtn, "Copy")
					btn.Inset = layout.Inset{Top: unit.Dp(2), Bottom: unit.Dp(2), Left: unit.Dp(8), Right: unit.Dp(8)}
					return btn.Layout(gtx)
				}),
				layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
				layout.Rigid(windowButton(ui.th, &ui.minimizeBtn, "-")),
				layout.Rigid(windowButton(ui.th, &ui.maximizeBtn, maximizeLabel(ui.maximized))),
				layout.Rigid(windowButton(ui.th, &ui.closeBtn, "x")),
			)
		})
	})
}

func (ui *uiState) confirmDeleteLayer(gtx layout.Context) layout.Dimensions {
	paint.FillShape(gtx.Ops, color.NRGBA{R: 18, G: 24, B: 34, A: 120}, clip.Rect{Max: gtx.Constraints.Max}.Op())
	gtx.Constraints.Min.X = 0
	gtx.Constraints.Min.Y = 0
	gtx.Constraints.Max.X = min(gtx.Constraints.Max.X, gtx.Dp(unit.Dp(360)))
	gtx.Constraints.Max.Y = min(gtx.Constraints.Max.Y, gtx.Dp(unit.Dp(180)))
	return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return panel(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.UniformInset(unit.Dp(16)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				name := ""
				if ui.pendingDelete >= 0 && ui.pendingDelete < len(ui.servers) {
					name = ui.servers[ui.pendingDelete].Name
				}
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						label := material.Body1(ui.th, "Delete server?")
						label.Color = color.NRGBA{R: 22, G: 27, B: 36, A: 255}
						return label.Layout(gtx)
					}),
					layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
					layout.Rigid(muted(ui.th, name)),
					layout.Rigid(layout.Spacer{Height: unit.Dp(16)}.Layout),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
							layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
								return material.Button(ui.th, &ui.cancelDeleteBtn, "Cancel").Layout(gtx)
							}),
							layout.Rigid(layout.Spacer{Width: unit.Dp(10)}.Layout),
							layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
								btn := material.Button(ui.th, &ui.confirmDeleteBtn, "Delete")
								btn.Background = color.NRGBA{R: 176, G: 54, B: 54, A: 255}
								return btn.Layout(gtx)
							}),
						)
					}),
				)
			})
		})
	})
}

func (ui *uiState) serverList(gtx layout.Context) layout.Dimensions {
	return panel(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.UniformInset(unit.Dp(14)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			children := []layout.FlexChild{
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
						layout.Flexed(1, sectionTitle(ui.th, "Servers")),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return material.Button(ui.th, &ui.addBtn, "Add").Layout(gtx)
						}),
					)
				}),
				layout.Rigid(layout.Spacer{Height: unit.Dp(10)}.Layout),
			}
			if len(ui.servers) == 0 {
				children = append(children, layout.Rigid(muted(ui.th, "No servers")))
			}
			for i := range ui.servers {
				idx := i
				children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return ui.serverRow(gtx, idx)
				}))
			}
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
		})
	})
}

func (ui *uiState) serverRow(gtx layout.Context, idx int) layout.Dimensions {
	server := ui.servers[idx]
	bg := color.NRGBA{R: 24, G: 25, B: 28, A: 255}
	if idx == ui.selectedServer {
		bg = color.NRGBA{R: 42, G: 45, B: 51, A: 255}
	}
	return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return surface(gtx, bg, func(gtx layout.Context) layout.Dimensions {
					return layout.UniformInset(unit.Dp(10)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
							layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
								return ui.serverClicks[idx].Layout(gtx, func(gtx layout.Context) layout.Dimensions {
									return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
										layout.Rigid(func(gtx layout.Context) layout.Dimensions {
											label := material.Body1(ui.th, server.Name)
											label.Color = color.NRGBA{R: 232, G: 234, B: 237, A: 255}
											return label.Layout(gtx)
										}),
										layout.Rigid(func(gtx layout.Context) layout.Dimensions {
											label := material.Caption(ui.th, serverSubtitle(server))
											label.Color = color.NRGBA{R: 143, G: 149, B: 160, A: 255}
											return label.Layout(gtx)
										}),
										layout.Rigid(func(gtx layout.Context) layout.Dimensions {
											note := serverNotesPreview(server.Notes)
											if note == "" {
												return layout.Dimensions{}
											}
											label := material.Caption(ui.th, note)
											label.Color = color.NRGBA{R: 112, G: 119, B: 132, A: 255}
											return label.Layout(gtx)
										}),
									)
								})
							}),
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								if isCapturedBastionServer(server) {
									return layout.Dimensions{}
								}
								return compactIconButton(ui.th, &ui.rowEditClicks[idx], "e")(gtx)
							}),
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								if isCapturedBastionServer(server) {
									return layout.Dimensions{}
								}
								return layout.Spacer{Width: unit.Dp(6)}.Layout(gtx)
							}),
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								return compactIconButton(ui.th, &ui.rowDeleteClicks[idx], "x")(gtx)
							}),
						)
					})
				})
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				if !ui.confirmDelete || ui.pendingDelete != idx {
					return layout.Dimensions{}
				}
				return ui.inlineDeleteConfirm(gtx, server.Name)
			}),
		)
	})
}

func (ui *uiState) inlineDeleteConfirm(gtx layout.Context, name string) layout.Dimensions {
	return layout.Inset{Top: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return surface(gtx, color.NRGBA{R: 255, G: 244, B: 229, A: 255}, func(gtx layout.Context) layout.Dimensions {
			return layout.UniformInset(unit.Dp(8)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
					layout.Flexed(1, muted(ui.th, "Delete "+name+"?")),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return material.Button(ui.th, &ui.cancelDeleteBtn, "Cancel").Layout(gtx)
					}),
					layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						btn := material.Button(ui.th, &ui.confirmDeleteBtn, "Delete")
						btn.Background = color.NRGBA{R: 176, G: 54, B: 54, A: 255}
						return btn.Layout(gtx)
					}),
				)
			})
		})
	})
}

func (ui *uiState) editor(gtx layout.Context) layout.Dimensions {
	return panel(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.UniformInset(unit.Dp(16)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			addressLabel := "Address"
			passwordLabel := "Password"
			if ui.machineType == model.MachineTypeWindows {
				addressLabel = "Machine code"
				passwordLabel = "Fixed password"
			}
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(sectionTitle(ui.th, "Server")),
				layout.Rigid(layout.Spacer{Height: unit.Dp(12)}.Layout),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
						layout.Rigid(modeButton(ui.th, &ui.linuxModeBtn, "Linux", ui.machineType != model.MachineTypeWindows)),
						layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
						layout.Rigid(modeButton(ui.th, &ui.windowsModeBtn, "Windows", ui.machineType == model.MachineTypeWindows)),
					)
				}),
				layout.Rigid(layout.Spacer{Height: unit.Dp(14)}.Layout),
				layout.Rigid(ui.field("Name", &ui.name)),
				layout.Rigid(ui.field(addressLabel, &ui.address)),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					if ui.machineType == model.MachineTypeWindows {
						return layout.Dimensions{}
					}
					return ui.field("Port", &ui.port)(gtx)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					if ui.machineType == model.MachineTypeWindows {
						return layout.Dimensions{}
					}
					return ui.field("Username", &ui.username)(gtx)
				}),
				layout.Rigid(ui.field(passwordLabel, &ui.password)),
				layout.Rigid(ui.field("Notes / projects", &ui.notes)),
				layout.Rigid(layout.Spacer{Height: unit.Dp(14)}.Layout),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return material.Button(ui.th, &ui.saveBtn, "Save").Layout(gtx)
				}),
			)
		})
	})
}

func (ui *uiState) logs(gtx layout.Context) layout.Dimensions {
	return terminalPanel(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Top: unit.Dp(10), Bottom: unit.Dp(12), Left: unit.Dp(12), Right: unit.Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			children := []layout.FlexChild{}
			if len(ui.terminalTabs) > 0 {
				children = append(children,
					layout.Rigid(ui.terminalTabsBar),
					layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
					layout.Rigid(ui.terminalSessionBar),
					layout.Rigid(layout.Spacer{Height: unit.Dp(10)}.Layout),
				)
			}
			lines := []terminalLine{}
			if len(ui.terminalTabs) > 0 {
				lines = ui.terminalTabs[ui.activeTerminal].Lines
				if len(lines) == 0 && len(ui.terminalTabs[ui.activeTerminal].Commands) > 0 {
					lines = terminalLines(ui.terminalTabs[ui.activeTerminal].Commands)
					ui.terminalTabs[ui.activeTerminal].Lines = lines
				}
			}
			children = append(children, layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				return surface(gtx, color.NRGBA{R: 2, G: 8, B: 13, A: 255}, func(gtx layout.Context) layout.Dimensions {
					return layout.UniformInset(unit.Dp(12)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
							layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
								if len(ui.terminalTabs) == 0 {
									return layout.Dimensions{Size: gtx.Constraints.Min}
								}
								tab := &ui.terminalTabs[ui.activeTerminal]
								list := material.List(ui.th, &tab.HistoryList)
								list.AnchorStrategy = material.Occupy
								list.ScrollbarStyle.Indicator.MinorWidth = unit.Dp(5)
								list.ScrollbarStyle.Indicator.Color = color.NRGBA{R: 66, G: 82, B: 102, A: 210}
								list.ScrollbarStyle.Indicator.HoverColor = color.NRGBA{R: 96, G: 116, B: 140, A: 240}
								list.ScrollbarStyle.Track.Color = color.NRGBA{R: 9, G: 18, B: 26, A: 120}
								return list.Layout(gtx, len(lines), func(gtx layout.Context, index int) layout.Dimensions {
									return terminalLineText(ui.th, lines[index])(gtx)
								})
							}),
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								if len(ui.terminalTabs) == 0 {
									return layout.Dimensions{}
								}
								if ui.focusTerminalInput {
									gtx.Execute(key.FocusCmd{Tag: &ui.terminalTabs[ui.activeTerminal].CommandInput})
									ui.focusTerminalInput = false
								}
								return layout.Inset{Top: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
									return terminalInput(ui.th, &ui.terminalTabs[ui.activeTerminal].CommandInput, "type command and press Enter")(gtx)
								})
							}),
						)
					})
				})
			}))
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
		})
	})
}

type terminalLine struct {
	segments []terminalSegment
}

type terminalSegment struct {
	text  string
	color color.NRGBA
}

func terminalLines(commands []model.Command) []terminalLine {
	lines := make([]terminalLine, 0, len(commands)*4)
	for _, entry := range commands {
		lines = append(lines, plainTerminalLine("root@opsera:~$ "+entry.Command, color.NRGBA{R: 246, G: 248, B: 250, A: 255}))
		lines = append(lines, splitTerminalOutput(entry.Output, color.NRGBA{R: 206, G: 213, B: 222, A: 255})...)
		if strings.TrimSpace(entry.Error) != "" {
			lines = append(lines, splitTerminalOutput("ERROR: "+entry.Error, color.NRGBA{R: 255, G: 92, B: 92, A: 255})...)
		}
		lines = append(lines, plainTerminalLine("", color.NRGBA{A: 255}))
	}
	return lines
}

func plainTerminalLine(value string, c color.NRGBA) terminalLine {
	return terminalLine{segments: []terminalSegment{{text: value, color: c}}}
}

func splitTerminalOutput(value string, base color.NRGBA) []terminalLine {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	value = strings.TrimRight(value, "\n")
	if value == "" {
		return nil
	}
	rawLines := strings.Split(value, "\n")
	lines := make([]terminalLine, 0, len(rawLines))
	for _, raw := range rawLines {
		lines = append(lines, terminalLine{segments: parseANSISegments(raw, base)})
	}
	return lines
}

func (ui *uiState) terminalTabsBar(gtx layout.Context) layout.Dimensions {
	children := []layout.FlexChild{}
	for i := range ui.terminalTabs {
		idx := i
		children = append(children,
			layout.Rigid(terminalTabButton(ui.th, &ui.terminalTabClicks[idx], ui.terminalTabs[idx].Title, idx == ui.activeTerminal)),
			layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
			layout.Rigid(terminalTabButton(ui.th, &ui.closeTabClicks[idx], "x", false)),
			layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
		)
	}
	return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx, children...)
}

func (ui *uiState) terminalSessionBar(gtx layout.Context) layout.Dimensions {
	target := "no session"
	if ui.activeTerminal >= 0 && ui.activeTerminal < len(ui.terminalTabs) {
		tab := ui.terminalTabs[ui.activeTerminal]
		target = tab.Target
		if strings.TrimSpace(target) == "" {
			target = tab.Title
		}
	}
	return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			label := material.Caption(ui.th, "SESSION")
			label.Color = color.NRGBA{R: 105, G: 219, B: 124, A: 255}
			return label.Layout(gtx)
		}),
		layout.Rigid(layout.Spacer{Width: unit.Dp(10)}.Layout),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			label := material.Caption(ui.th, target)
			label.Color = color.NRGBA{R: 169, G: 178, B: 191, A: 255}
			return label.Layout(gtx)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			label := material.Caption(ui.th, "Enter to run")
			label.Color = color.NRGBA{R: 96, G: 106, B: 122, A: 255}
			return label.Layout(gtx)
		}),
	)
}

func (ui *uiState) runCommand(text string) {
	if len(ui.terminalTabs) == 0 {
		if sshSession, err := opcli.LatestSession(""); err == nil && sshSession.Host != "" {
			ui.addTerminalTab(sshSession.Host, sshSession.Host)
		} else {
			ui.status = "No active tunnel"
			return
		}
	}
	tab := &ui.terminalTabs[ui.activeTerminal]
	command := strings.TrimSpace(text)
	if command == "" {
		ui.status = "Command is required"
		return
	}
	tab.History = appendCommandHistory(tab.History, command)
	tab.HistoryIndex = len(tab.History)
	commandID := fmt.Sprintf("cmd-%d", time.Now().UnixNano())
	item := model.Command{
		ID:        commandID,
		Command:   command,
		Status:    model.CommandStatusQueued,
		CreatedAt: time.Now().Format(time.RFC3339),
		UpdatedAt: time.Now().Format(time.RFC3339),
	}
	tab.Commands = append(tab.Commands, item)
	tab.Lines = terminalLines(tab.Commands)
	tab.HistoryList.List.Position.BeforeEnd = false
	tab.CommandInput.SetText("")
	ui.status = "Ready"
	ui.window.Invalidate()
	tabID := tab.ID
	serverID := ui.resolveTerminalCommandServerID(tab)
	go func() {
		output, err := ui.runTerminalCommand(serverID, command)
		ui.commandResults <- terminalCommandResult{
			tabID:     tabID,
			commandID: commandID,
			output:    output,
			err:       err,
		}
		ui.window.Invalidate()
	}()
}

func (ui *uiState) navigateCommandHistory(tabIndex int, previous bool) {
	if tabIndex < 0 || tabIndex >= len(ui.terminalTabs) {
		return
	}
	tab := &ui.terminalTabs[tabIndex]
	if len(tab.History) == 0 {
		return
	}
	if tab.HistoryIndex < 0 || tab.HistoryIndex > len(tab.History) {
		tab.HistoryIndex = len(tab.History)
	}
	if previous {
		if tab.HistoryIndex > 0 {
			tab.HistoryIndex--
		}
	} else if tab.HistoryIndex < len(tab.History) {
		tab.HistoryIndex++
	}
	if tab.HistoryIndex >= len(tab.History) {
		tab.CommandInput.SetText("")
		return
	}
	tab.CommandInput.SetText(tab.History[tab.HistoryIndex])
}

func appendCommandHistory(history []string, command string) []string {
	if len(history) > 0 && history[len(history)-1] == command {
		return history
	}
	return append(history, command)
}

func (ui *uiState) completeCommandInput(tabIndex int) {
	if tabIndex < 0 || tabIndex >= len(ui.terminalTabs) {
		return
	}
	tab := &ui.terminalTabs[tabIndex]
	current := tab.CommandInput.Text()
	prefix := strings.TrimLeft(current, " \t")
	if prefix == "" {
		return
	}
	matches := commandCompletionMatches(prefix, tab.History)
	if len(matches) == 0 {
		return
	}
	completion := longestCommonPrefix(matches)
	if completion == "" {
		return
	}
	leading := current[:len(current)-len(prefix)]
	tab.CommandInput.SetText(leading + completion)
}

func commandCompletionMatches(prefix string, history []string) []string {
	seen := map[string]bool{}
	matches := []string{}
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] || !strings.HasPrefix(value, prefix) {
			return
		}
		seen[value] = true
		matches = append(matches, value)
	}
	for i := len(history) - 1; i >= 0; i-- {
		add(history[i])
	}
	for _, value := range commonTerminalCommands {
		add(value)
	}
	return matches
}

func longestCommonPrefix(values []string) string {
	if len(values) == 0 {
		return ""
	}
	prefix := values[0]
	for _, value := range values[1:] {
		for !strings.HasPrefix(value, prefix) && prefix != "" {
			prefix = prefix[:len(prefix)-1]
		}
		if prefix == "" {
			return ""
		}
	}
	return prefix
}

var commonTerminalCommands = []string{
	"cat", "cd", "chmod", "chown", "cp", "df -h", "du -sh", "free -m", "grep",
	"head", "journalctl", "less", "ll", "ll -h", "ls", "ls -lah", "mkdir", "mv",
	"ps aux", "pwd", "rm", "sed", "service", "systemctl status", "tail -f", "tar",
	"top", "vim", "vi", "which",
}

func (ui *uiState) resolveTerminalCommandServerID(tab *terminalTab) string {
	if strings.TrimSpace(tab.ServerID) != "" {
		if server, ok := ui.findServerByID(tab.ServerID); ok && strings.TrimSpace(server.ID) != "" {
			return tab.ServerID
		}
	}
	if ui.selectedServer >= 0 && ui.selectedServer < len(ui.servers) {
		server := ui.servers[ui.selectedServer]
		if strings.TrimSpace(server.ID) != "" {
			return server.ID
		}
	}
	return ""
}

func (ui *uiState) runTerminalCommand(serverID string, command string) (string, error) {
	if strings.TrimSpace(serverID) != "" {
		if server, ok := ui.findServerByID(serverID); ok {
			if isDirectSSHServer(server) {
				return ui.rt.sshPool.Run(serverID, command)
			}
			return ui.rt.runOpenSessionCommand(server.Host, command)
		}
	}
	return opcli.RunCommand("", command)
}

func (ui *uiState) findServerByID(id string) (model.Server, bool) {
	for _, server := range ui.servers {
		if server.ID == id {
			return server, true
		}
	}
	return model.Server{}, false
}

func isDirectSSHServer(server model.Server) bool {
	if server.MachineType == model.MachineTypeWindows || server.Mode == model.ConnectionModeRelayAgent {
		return false
	}
	return server.Mode == "" || server.Mode == model.ConnectionModeDirectSSH
}

func isCapturedBastionServer(server model.Server) bool {
	return server.Mode == model.ConnectionModeVPNLauncher
}

func (ui *uiState) relayShareText() string {
	return fmt.Sprintf("Code: %s\nPassword: %s", ui.rt.identity.MachineCode, ui.rt.identity.Password)
}

func ensureTrayIconFile(dataDir string) error {
	target := filepath.Join(dataDir, "opsera.ico")
	if info, err := os.Stat(target); err == nil && info.Size() > 1024 {
		return nil
	}
	candidates := []string{}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, "opsera.ico"),
			filepath.Join(dir, "assets", "opsera.ico"),
		)
	}
	candidates = append(candidates, filepath.Join("assets", "opsera.ico"))
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err != nil {
			continue
		}
		input, err := os.Open(candidate)
		if err != nil {
			continue
		}
		defer input.Close()
		output, err := os.Create(target)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(output, input)
		closeErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	}
	return os.ErrNotExist
}

func (ui *uiState) field(labelText string, editor *widget.Editor) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Bottom: unit.Dp(10)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					label := material.Caption(ui.th, labelText)
					label.Color = color.NRGBA{R: 84, G: 91, B: 106, A: 255}
					return label.Layout(gtx)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return surface(gtx, color.NRGBA{R: 14, G: 15, B: 18, A: 255}, func(gtx layout.Context) layout.Dimensions {
						return layout.UniformInset(unit.Dp(8)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							ed := material.Editor(ui.th, editor, "")
							ed.Color = color.NRGBA{R: 232, G: 234, B: 237, A: 255}
							ed.HintColor = color.NRGBA{R: 110, G: 116, B: 128, A: 255}
							return ed.Layout(gtx)
						})
					})
				}),
			)
		})
	}
}

func (ui *uiState) clearForm() {
	ui.selectedServer = -1
	ui.editing = true
	ui.machineType = model.MachineTypeLinux
	ui.name.SetText("")
	ui.address.SetText("")
	ui.port.SetText("22")
	ui.username.SetText("")
	ui.password.SetText("")
	ui.notes.SetText("")
	ui.status = "New server"
}

func (ui *uiState) load(server model.Server) {
	ui.machineType = server.MachineType
	if ui.machineType == "" {
		ui.machineType = model.MachineTypeLinux
	}
	ui.name.SetText(server.Name)
	ui.notes.SetText(server.Notes)
	if ui.machineType == model.MachineTypeWindows {
		ui.address.SetText(server.MachineCode)
		ui.port.SetText("")
	} else {
		ui.address.SetText(server.Host)
		if server.Port > 0 {
			ui.port.SetText(strconv.Itoa(server.Port))
		} else {
			ui.port.SetText("22")
		}
	}
	ui.password.SetText("")
	ui.username.SetText("")
	for _, credential := range ui.credentials {
		if credential.ID == server.CredentialRef {
			ui.username.SetText(credential.Username)
			if credential.SecretCipher != "" {
				plain, err := ui.rt.vault.Decrypt(credential.SecretCipher)
				if err != nil {
					ui.status = "Could not decrypt password: " + err.Error()
				} else {
					ui.password.SetText(plain)
				}
			}
			break
		}
	}
}

func (ui *uiState) save() {
	name := strings.TrimSpace(ui.name.Text())
	address := strings.TrimSpace(ui.address.Text())
	portText := strings.TrimSpace(ui.port.Text())
	username := strings.TrimSpace(ui.username.Text())
	password := ui.password.Text()
	notes := strings.TrimSpace(ui.notes.Text())
	port := 22
	if ui.machineType == "" {
		ui.machineType = model.MachineTypeLinux
	}
	if ui.machineType == model.MachineTypeWindows {
		if name == "" || address == "" {
			ui.status = "Name and machine code are required"
			return
		}
		username = address
	} else {
		if name == "" || address == "" || username == "" {
			ui.status = "Name, address and username are required"
			return
		}
		if portText != "" {
			parsed, err := strconv.Atoi(portText)
			if err != nil || parsed < 1 || parsed > 65535 {
				ui.status = "Port must be between 1 and 65535"
				return
			}
			port = parsed
		}
	}

	serverID := ""
	credentialID := ""
	secretCipher := ""
	if ui.selectedServer >= 0 && ui.selectedServer < len(ui.servers) {
		current := ui.servers[ui.selectedServer]
		serverID = current.ID
		credentialID = current.CredentialRef
		for _, credential := range ui.credentials {
			if credential.ID == credentialID {
				secretCipher = credential.SecretCipher
				break
			}
		}
	}
	if serverID == "" {
		serverID = fmt.Sprintf("srv-%d", time.Now().UnixNano())
	}
	if credentialID == "" {
		credentialID = fmt.Sprintf("cred-%d", time.Now().UnixNano())
	}
	if password != "" {
		encrypted, err := ui.rt.vault.Encrypt(password)
		if err != nil {
			ui.status = err.Error()
			return
		}
		secretCipher = encrypted
	}
	if secretCipher == "" {
		ui.status = "Password is required"
		return
	}

	if err := ui.rt.store.UpsertCredential(model.Credential{
		ID:           credentialID,
		Name:         name,
		Username:     username,
		SecretCipher: secretCipher,
	}); err != nil {
		ui.status = err.Error()
		return
	}
	server := model.Server{
		ID:            serverID,
		Name:          name,
		Host:          address,
		CredentialRef: credentialID,
		MachineType:   ui.machineType,
		Notes:         notes,
	}
	if ui.machineType == model.MachineTypeWindows {
		server.Host = address
		server.MachineCode = address
		server.Mode = model.ConnectionModeRelayAgent
	} else {
		server.Port = port
		server.Mode = model.ConnectionModeDirectSSH
	}
	if err := ui.rt.store.UpsertServer(server); err != nil {
		ui.status = err.Error()
		return
	}
	_ = ui.rt.logStore.Append(model.LogLevelInfo, "ui", "server saved: "+name, serverID)
	ui.reload()
	ui.editing = false
	ui.status = "Saved"
}

func (ui *uiState) delete() {
	if ui.selectedServer < 0 || ui.selectedServer >= len(ui.servers) {
		ui.status = "Select a server"
		return
	}
	server := ui.servers[ui.selectedServer]
	if err := ui.rt.store.DeleteServer(server.ID); err != nil {
		ui.status = err.Error()
		return
	}
	if server.CredentialRef != "" {
		_ = ui.rt.store.DeleteCredential(server.CredentialRef)
	}
	_ = ui.rt.logStore.Append(model.LogLevelWarn, "ui", "server deleted: "+server.Name, server.ID)
	ui.clearForm()
	ui.editing = false
	ui.reload()
	ui.status = "Deleted"
}

func resolveDataDir() (string, error) {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base = os.Getenv("HOME")
	}
	if base == "" {
		return "", fmt.Errorf("no user data directory available")
	}
	root := filepath.Join(base, "Opsera")
	return root, os.MkdirAll(root, 0o755)
}

func loadLocalIdentity(dataDir string) (localIdentity, error) {
	path := filepath.Join(dataDir, "identity.json")
	raw, err := os.ReadFile(path)
	if err == nil && len(raw) > 0 {
		var identity localIdentity
		if err := json.Unmarshal(raw, &identity); err != nil {
			return localIdentity{}, err
		}
		if strings.TrimSpace(identity.MachineCode) != "" && strings.TrimSpace(identity.Password) != "" {
			if strings.TrimSpace(identity.Secret) == "" {
				identity.Secret = randomSecret(32)
				if err := saveLocalIdentity(path, identity); err != nil {
					return localIdentity{}, err
				}
			}
			return identity, nil
		}
	}
	if err != nil && !os.IsNotExist(err) {
		return localIdentity{}, err
	}
	identity := localIdentity{
		MachineCode: randomDigits(9),
		Password:    randomDigits(6),
		Secret:      randomSecret(32),
	}
	if err := saveLocalIdentity(path, identity); err != nil {
		return localIdentity{}, err
	}
	return identity, nil
}

func saveLocalIdentity(path string, identity localIdentity) error {
	out, err := json.MarshalIndent(identity, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return err
	}
	return nil
}

func loadRelayConfig(dataDir string) (relayConfig, error) {
	host := strings.TrimSpace(os.Getenv("OPSERA_RELAY_HOST"))
	if host != "" {
		return relayConfig{Host: host}, nil
	}
	paths := []string{}
	if exe, err := os.Executable(); err == nil {
		paths = append(paths, filepath.Join(filepath.Dir(exe), "relay-config.json"))
	}
	paths = append(paths, filepath.Join(dataDir, "relay-config.json"))
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err == nil && len(raw) > 0 {
			var config relayConfig
			if err := json.Unmarshal(raw, &config); err != nil {
				return relayConfig{}, err
			}
			config.Host = strings.TrimSpace(config.Host)
			if config.Host != "" {
				return config, nil
			}
			continue
		}
		if err != nil && !os.IsNotExist(err) {
			return relayConfig{}, err
		}
	}
	return relayConfig{}, nil
}

func randomDigits(length int) string {
	if length <= 0 {
		return ""
	}
	buf := make([]byte, length)
	random := make([]byte, length)
	if _, err := rand.Read(random); err != nil {
		for i := range buf {
			buf[i] = byte('0' + time.Now().UnixNano()%10)
		}
		return string(buf)
	}
	for i := range buf {
		buf[i] = byte('0' + random[i]%10)
	}
	if length > 0 && buf[0] == '0' {
		buf[0] = '1'
	}
	return string(buf)
}

func randomSecret(length int) string {
	if length < 24 {
		length = 24
	}
	buf := make([]byte, length)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d%s", time.Now().UnixNano(), randomDigits(24))
	}
	return fmt.Sprintf("%x", buf)
}

func panel(gtx layout.Context, w layout.Widget) layout.Dimensions {
	return surface(gtx, color.NRGBA{R: 21, G: 22, B: 26, A: 255}, w)
}

func terminalPanel(gtx layout.Context, w layout.Widget) layout.Dimensions {
	return surface(gtx, color.NRGBA{R: 5, G: 8, B: 12, A: 255}, w)
}

func surface(gtx layout.Context, bg color.NRGBA, w layout.Widget) layout.Dimensions {
	macro := op.Record(gtx.Ops)
	dims := w(gtx)
	call := macro.Stop()
	paint.FillShape(gtx.Ops, bg, clip.Rect{Max: dims.Size}.Op())
	call.Add(gtx.Ops)
	return dims
}

func fill(gtx layout.Context, c color.NRGBA) {
	paint.FillShape(gtx.Ops, c, clip.Rect{Max: gtx.Constraints.Max}.Op())
}

func sectionTitle(th *material.Theme, textValue string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		label := material.Body1(th, textValue)
		label.Color = color.NRGBA{R: 232, G: 234, B: 237, A: 255}
		label.Alignment = text.Start
		return label.Layout(gtx)
	}
}

func muted(th *material.Theme, textValue string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		label := material.Caption(th, textValue)
		label.Color = color.NRGBA{R: 143, G: 149, B: 160, A: 255}
		return label.Layout(gtx)
	}
}

func terminalText(th *material.Theme, textValue string, c color.NRGBA) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		label := material.Body2(th, stripANSI(textValue))
		label.Font.Typeface = terminalTypeface
		label.TextSize = unit.Sp(14)
		label.LineHeight = unit.Sp(18)
		label.Color = c
		return label.Layout(gtx)
	}
}

func terminalLineText(th *material.Theme, line terminalLine) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		children := make([]layout.FlexChild, 0, len(line.segments))
		for _, segment := range line.segments {
			segment := segment
			children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				label := material.Body2(th, segment.text)
				label.Font.Typeface = terminalTypeface
				label.TextSize = unit.Sp(14)
				label.LineHeight = unit.Sp(18)
				label.MaxLines = 1
				label.Color = segment.color
				return label.Layout(gtx)
			}))
		}
		return layout.Flex{Axis: layout.Horizontal}.Layout(gtx, children...)
	}
}

const terminalTypeface = font.Typeface("Cascadia Mono, Consolas, monospace")

var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
var ansiSGRPattern = regexp.MustCompile(`\x1b\[([0-9;]*)m`)

func stripANSI(value string) string {
	return ansiPattern.ReplaceAllString(value, "")
}

func parseANSISegments(value string, base color.NRGBA) []terminalSegment {
	segments := []terminalSegment{}
	current := base
	last := 0
	for _, match := range ansiSGRPattern.FindAllStringSubmatchIndex(value, -1) {
		if match[0] > last {
			segments = appendTerminalSegment(segments, value[last:match[0]], current)
		}
		params := ""
		if match[2] >= 0 && match[3] >= 0 {
			params = value[match[2]:match[3]]
		}
		current = applySGRColor(current, base, params)
		last = match[1]
	}
	if last < len(value) {
		segments = appendTerminalSegment(segments, value[last:], current)
	}
	if len(segments) == 0 {
		return []terminalSegment{{text: stripANSI(value), color: base}}
	}
	return segments
}

func appendTerminalSegment(segments []terminalSegment, value string, c color.NRGBA) []terminalSegment {
	value = stripANSI(value)
	if value == "" {
		return segments
	}
	if len(segments) > 0 && segments[len(segments)-1].color == c {
		segments[len(segments)-1].text += value
		return segments
	}
	return append(segments, terminalSegment{text: value, color: c})
}

func applySGRColor(current color.NRGBA, base color.NRGBA, params string) color.NRGBA {
	if params == "" {
		return base
	}
	parts := strings.Split(params, ";")
	for i := 0; i < len(parts); i++ {
		code, err := strconv.Atoi(parts[i])
		if err != nil {
			continue
		}
		switch {
		case code == 0 || code == 39:
			current = base
		case code >= 30 && code <= 37:
			current = ansiBasicColor(code - 30)
		case code >= 90 && code <= 97:
			current = ansiBrightColor(code - 90)
		case code == 38 && i+2 < len(parts) && parts[i+1] == "5":
			if idx, err := strconv.Atoi(parts[i+2]); err == nil {
				current = ansi256Color(idx)
				i += 2
			}
		}
	}
	return current
}

func ansiBasicColor(index int) color.NRGBA {
	palette := []color.NRGBA{
		{R: 16, G: 24, B: 32, A: 255},
		{R: 255, G: 96, B: 96, A: 255},
		{R: 105, G: 219, B: 124, A: 255},
		{R: 238, G: 196, B: 92, A: 255},
		{R: 91, G: 173, B: 255, A: 255},
		{R: 214, G: 126, B: 255, A: 255},
		{R: 62, G: 207, B: 202, A: 255},
		{R: 221, G: 226, B: 234, A: 255},
	}
	return palette[index]
}

func ansiBrightColor(index int) color.NRGBA {
	palette := []color.NRGBA{
		{R: 91, G: 104, B: 119, A: 255},
		{R: 255, G: 92, B: 92, A: 255},
		{R: 129, G: 230, B: 145, A: 255},
		{R: 255, G: 213, B: 102, A: 255},
		{R: 94, G: 190, B: 255, A: 255},
		{R: 225, G: 151, B: 255, A: 255},
		{R: 96, G: 230, B: 226, A: 255},
		{R: 246, G: 248, B: 250, A: 255},
	}
	return palette[index]
}

func ansi256Color(index int) color.NRGBA {
	if index < 16 {
		if index < 8 {
			return ansiBasicColor(index)
		}
		return ansiBrightColor(index - 8)
	}
	if index >= 16 && index <= 231 {
		idx := index - 16
		r := idx / 36
		g := (idx / 6) % 6
		b := idx % 6
		scale := func(v int) uint8 {
			if v == 0 {
				return 0
			}
			return uint8(55 + v*40)
		}
		return color.NRGBA{R: scale(r), G: scale(g), B: scale(b), A: 255}
	}
	if index >= 232 && index <= 255 {
		v := uint8(8 + (index-232)*10)
		return color.NRGBA{R: v, G: v, B: v, A: 255}
	}
	return color.NRGBA{R: 206, G: 213, B: 222, A: 255}
}

func terminalPromptLine(th *material.Theme, command string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				label := material.Body2(th, "root@opsera")
				label.Font.Typeface = terminalTypeface
				label.TextSize = unit.Sp(14)
				label.Color = color.NRGBA{R: 98, G: 222, B: 118, A: 255}
				return label.Layout(gtx)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				label := material.Body2(th, ":~$ ")
				label.Font.Typeface = terminalTypeface
				label.TextSize = unit.Sp(14)
				label.Color = color.NRGBA{R: 91, G: 173, B: 255, A: 255}
				return label.Layout(gtx)
			}),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				label := material.Body2(th, command)
				label.Font.Typeface = terminalTypeface
				label.TextSize = unit.Sp(14)
				label.Color = color.NRGBA{R: 246, G: 248, B: 250, A: 255}
				return label.Layout(gtx)
			}),
		)
	}
}

func terminalInput(th *material.Theme, editor *widget.Editor, hint string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Top: unit.Dp(2), Bottom: unit.Dp(2)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					label := material.Body1(th, "root@opsera")
					label.Font.Typeface = terminalTypeface
					label.TextSize = unit.Sp(14)
					label.Color = color.NRGBA{R: 98, G: 222, B: 118, A: 255}
					return label.Layout(gtx)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					label := material.Body1(th, ":~$ ")
					label.Font.Typeface = terminalTypeface
					label.TextSize = unit.Sp(14)
					label.Color = color.NRGBA{R: 91, G: 173, B: 255, A: 255}
					return label.Layout(gtx)
				}),
				layout.Rigid(layout.Spacer{Width: unit.Dp(4)}.Layout),
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					ed := material.Editor(th, editor, hint)
					ed.Font.Typeface = terminalTypeface
					ed.TextSize = unit.Sp(14)
					ed.Color = color.NRGBA{R: 247, G: 250, B: 252, A: 255}
					ed.HintColor = color.NRGBA{R: 82, G: 96, B: 112, A: 255}
					return ed.Layout(gtx)
				}),
			)
		})
	}
}

func logoMark(th *material.Theme) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		size := gtx.Dp(unit.Dp(28))
		gtx.Constraints.Min.X = size
		gtx.Constraints.Min.Y = size
		gtx.Constraints.Max.X = size
		gtx.Constraints.Max.Y = size
		bg := clip.RRect{
			Rect: image.Rectangle{Max: image.Pt(size, size)},
			NW:   gtx.Dp(unit.Dp(6)),
			NE:   gtx.Dp(unit.Dp(6)),
			SW:   gtx.Dp(unit.Dp(6)),
			SE:   gtx.Dp(unit.Dp(6)),
		}.Op(gtx.Ops)
		paint.FillShape(gtx.Ops, color.NRGBA{R: 11, G: 20, B: 18, A: 255}, bg)
		ringInset := gtx.Dp(unit.Dp(7))
		ring := clip.Ellipse{Min: image.Pt(ringInset, ringInset), Max: image.Pt(size-ringInset, size-ringInset)}.Op(gtx.Ops)
		paint.FillShape(gtx.Ops, color.NRGBA{R: 72, G: 231, B: 132, A: 255}, ring)
		coreInset := gtx.Dp(unit.Dp(10))
		core := clip.Ellipse{Min: image.Pt(coreInset, coreInset), Max: image.Pt(size-coreInset, size-coreInset)}.Op(gtx.Ops)
		paint.FillShape(gtx.Ops, color.NRGBA{R: 2, G: 8, B: 13, A: 255}, core)
		accent := clip.Rect{Min: image.Pt(size-gtx.Dp(unit.Dp(9)), gtx.Dp(unit.Dp(5))), Max: image.Pt(size-gtx.Dp(unit.Dp(5)), gtx.Dp(unit.Dp(9)))}.Op()
		paint.FillShape(gtx.Ops, color.NRGBA{R: 91, G: 173, B: 255, A: 255}, accent)
		return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Dimensions{Size: image.Pt(size, size)}
		})
	}
}

func tabButton(th *material.Theme, click *widget.Clickable, label string, active bool) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		btn := material.Button(th, click, label)
		if active {
			btn.Background = color.NRGBA{R: 74, G: 111, B: 165, A: 255}
		} else {
			btn.Background = color.NRGBA{R: 35, G: 37, B: 43, A: 255}
			btn.Color = color.NRGBA{R: 189, G: 194, B: 203, A: 255}
		}
		return btn.Layout(gtx)
	}
}

func modeButton(th *material.Theme, click *widget.Clickable, label string, active bool) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		btn := material.Button(th, click, label)
		if active {
			btn.Background = color.NRGBA{R: 74, G: 111, B: 165, A: 255}
			btn.Color = color.NRGBA{R: 247, G: 249, B: 252, A: 255}
		} else {
			btn.Background = color.NRGBA{R: 35, G: 37, B: 43, A: 255}
			btn.Color = color.NRGBA{R: 189, G: 194, B: 203, A: 255}
		}
		return btn.Layout(gtx)
	}
}

func serverSubtitle(server model.Server) string {
	if server.MachineType == model.MachineTypeWindows || server.Mode == model.ConnectionModeRelayAgent {
		code := strings.TrimSpace(server.MachineCode)
		if code == "" {
			code = server.Host
		}
		if code == "" {
			return "Windows relay"
		}
		return "Windows relay - " + code
	}
	if strings.TrimSpace(server.Host) == "" {
		return "Linux SSH"
	}
	return "Linux SSH - " + server.Host
}

func serverNotesPreview(notes string) string {
	lines := strings.Fields(strings.ReplaceAll(notes, "\r", "\n"))
	if len(lines) == 0 {
		return ""
	}
	preview := strings.Join(lines, " ")
	if len(preview) > 72 {
		preview = preview[:72] + "..."
	}
	return preview
}

func terminalTabButton(th *material.Theme, click *widget.Clickable, label string, active bool) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		btn := material.Button(th, click, label)
		if active {
			btn.Background = color.NRGBA{R: 7, G: 40, B: 34, A: 255}
			btn.Color = color.NRGBA{R: 214, G: 255, B: 224, A: 255}
		} else {
			btn.Background = color.NRGBA{R: 12, G: 18, B: 26, A: 255}
			btn.Color = color.NRGBA{R: 136, G: 148, B: 163, A: 255}
		}
		btn.Inset = layout.Inset{Top: unit.Dp(6), Bottom: unit.Dp(6), Left: unit.Dp(12), Right: unit.Dp(12)}
		return btn.Layout(gtx)
	}
}

func compactIconButton(th *material.Theme, click *widget.Clickable, label string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		size := gtx.Dp(unit.Dp(28))
		gtx.Constraints.Min.X = size
		gtx.Constraints.Max.X = size
		gtx.Constraints.Min.Y = size
		gtx.Constraints.Max.Y = size
		btn := material.Button(th, click, label)
		btn.Background = color.NRGBA{R: 48, G: 51, B: 58, A: 255}
		btn.Color = color.NRGBA{R: 213, G: 216, B: 222, A: 255}
		btn.TextSize = unit.Sp(12)
		btn.Inset = layout.Inset{Top: unit.Dp(1), Bottom: unit.Dp(1), Left: unit.Dp(1), Right: unit.Dp(1)}
		return btn.Layout(gtx)
	}
}

func windowButton(th *material.Theme, click *widget.Clickable, label string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		btn := material.Button(th, click, label)
		btn.Background = color.NRGBA{R: 10, G: 10, B: 10, A: 255}
		btn.Color = color.NRGBA{R: 218, G: 224, B: 232, A: 255}
		if label == "x" {
			btn.Color = color.NRGBA{R: 255, G: 120, B: 120, A: 255}
		}
		btn.Inset = layout.Inset{Top: unit.Dp(4), Bottom: unit.Dp(4), Left: unit.Dp(10), Right: unit.Dp(10)}
		return btn.Layout(gtx)
	}
}

func maximizeLabel(maximized bool) string {
	if maximized {
		return "[]"
	}
	return "[ ]"
}
