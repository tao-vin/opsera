package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/color"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gioui.org/app"
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
)

type runtime struct {
	store      *config.Store
	logStore   *logs.Store
	vault      *crypto.Vault
	sessions   *session.Manager
	commands   *session.CommandQueue
	httpServer *http.Server
	dataDir    string
	keepersMu  sync.Mutex
	keepers    []io.Closer
}

type uiState struct {
	rt *runtime
	th *material.Theme

	servers        []model.Server
	credentials    []model.Credential
	logItems       []model.LogEntry
	commandItems   []model.Command
	terminalTabs   []terminalTab
	activeTerminal int
	seenEvents     map[string]bool
	startedAt      time.Time
	lastEventPoll  time.Time
	lastConfigPoll time.Time

	selectedServer int
	editing        bool
	tab            string
	status         string
	confirmDelete  bool
	pendingDelete  int

	name     widget.Editor
	address  widget.Editor
	username widget.Editor
	password widget.Editor

	addBtn            widget.Clickable
	saveBtn           widget.Clickable
	serversTab        widget.Clickable
	logsTab           widget.Clickable
	terminalTabClicks []widget.Clickable
	closeTabClicks    []widget.Clickable
	serverClicks      []widget.Clickable
	rowEditClicks     []widget.Clickable
	rowDeleteClicks   []widget.Clickable
	confirmDeleteBtn  widget.Clickable
	cancelDeleteBtn   widget.Clickable
}

type terminalTab struct {
	ID           string
	Title        string
	Target       string
	CommandInput widget.Editor
	Commands     []model.Command
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
	rt := &runtime{
		store:    store,
		logStore: logStore,
		vault:    crypto.NewVault(dataDir),
		dataDir:  dataDir,
	}
	rt.sessions = session.NewManager(store, logStore)
	rt.commands = session.NewCommandQueue(logStore)
	if err := rt.startAPI(); err != nil {
		return err
	}
	rt.handleLaunchArgs(os.Args[1:])

	errCh := make(chan error, 1)
	go func() {
		window := new(app.Window)
		window.Option(app.Title("Opsera"))
		window.Option(app.Size(unit.Dp(1440), unit.Dp(900)))
		errCh <- rt.runWindow(window)
		os.Exit(0)
	}()
	app.Main()

	if rt.httpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = rt.httpServer.Shutdown(ctx)
	}
	select {
	case err := <-errCh:
		return err
	default:
		return nil
	}
}

func (r *runtime) startAPI() error {
	server := api.New(r.store, r.logStore, r.vault, r.sessions, r.commands, r.handleForwardedLaunch)
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
		return
	}
	r.ensureLaunchServer(target)
	r.sessions.Start(target, "argv")
	r.holdLaunchTunnel(args)
}

func (r *runtime) handleForwardedLaunch(args []string) {
	target := session.LaunchTarget(args)
	r.ensureLaunchServer(target)
	r.holdLaunchTunnel(args)
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
	_ = r.logStore.Append(model.LogLevelInfo, "ui", "vpn server added: "+target, id)
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

func forwardLaunch(args []string) bool {
	if len(args) == 0 {
		return false
	}
	raw, err := json.Marshal(map[string][]string{"args": args})
	if err != nil {
		return false
	}
	client := http.Client{Timeout: 800 * time.Millisecond}
	resp, err := client.Post("http://127.0.0.1:18741/launch", "application/json", bytes.NewReader(raw))
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func (r *runtime) runWindow(window *app.Window) error {
	ui := newUIState(r)
	var ops op.Ops
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(300 * time.Millisecond)
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
		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)
			ui.handle(gtx)
			ui.layout(gtx)
			e.Frame(gtx.Ops)
		}
	}
}

func newUIState(rt *runtime) *uiState {
	ui := &uiState{
		rt:             rt,
		th:             material.NewTheme(),
		selectedServer: -1,
		editing:        false,
		tab:            "servers",
		status:         "Ready",
		seenEvents:     map[string]bool{},
		startedAt:      time.Now(),
	}
	ui.name.SingleLine = true
	ui.address.SingleLine = true
	ui.username.SingleLine = true
	ui.password.SingleLine = true
	ui.password.Mask = '*'
	ui.reload()
	return ui
}

func (ui *uiState) addTerminalTab(title string, target string) {
	for i := range ui.terminalTabs {
		if ui.terminalTabs[i].Target == target && target != "" {
			ui.activeTerminal = i
			return
		}
	}
	tab := terminalTab{
		ID:     fmt.Sprintf("tab-%d", time.Now().UnixNano()),
		Title:  title,
		Target: target,
	}
	tab.CommandInput.SingleLine = true
	tab.CommandInput.Submit = true
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
	ui.pollEvents()
	ui.pollConfig()
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
				ui.runCommand(submit.Text)
			}
		}
	}
	for ui.saveBtn.Clicked(gtx) {
		ui.save()
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

func (ui *uiState) pollConfig() {
	if ui.editing || time.Since(ui.lastConfigPoll) < time.Second {
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
	for _, tab := range ui.terminalTabs {
		if tab.Target == target {
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
	if time.Since(ui.lastEventPoll) < 500*time.Millisecond {
		return
	}
	ui.lastEventPoll = time.Now()
	items, err := events.ReadAll(ui.rt.dataDir)
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
	ui.terminalTabs[idx].Commands = append([]model.Command{item}, ui.terminalTabs[idx].Commands...)
	ui.status = "Event: " + event.Type
}

func (ui *uiState) openServerTab(server model.Server) {
	title := server.Name
	if strings.TrimSpace(title) == "" {
		title = server.Host
	}
	ui.addTerminalTab(title, server.Host)
	ui.selectServerByTarget(server.Host)
}

func (ui *uiState) selectServerByTarget(target string) {
	for i := range ui.servers {
		if ui.servers[i].Host == target {
			ui.selectedServer = i
			return
		}
	}
}

func (ui *uiState) layout(gtx layout.Context) layout.Dimensions {
	fill(gtx, color.NRGBA{R: 247, G: 248, B: 250, A: 255})
	dims := layout.UniformInset(unit.Dp(18)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
					layout.Flexed(0.28, ui.serverList),
					layout.Rigid(layout.Spacer{Width: unit.Dp(14)}.Layout),
					layout.Flexed(0.72, func(gtx layout.Context) layout.Dimensions {
						if ui.editing {
							return ui.editor(gtx)
						}
						return ui.logs(gtx)
					}),
				)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
			layout.Rigid(ui.statusBar),
		)
	})
	return dims
}

func (ui *uiState) header(gtx layout.Context) layout.Dimensions {
	return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return logoMark(ui.th)(gtx)
				}),
				layout.Rigid(layout.Spacer{Width: unit.Dp(22)}.Layout),
				layout.Rigid(sectionTitle(ui.th, "Opsera")),
			)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Dimensions{}
		}),
	)
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
	bg := color.NRGBA{R: 255, G: 255, B: 255, A: 255}
	if idx == ui.selectedServer {
		bg = color.NRGBA{R: 224, G: 234, B: 247, A: 255}
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
											label.Color = color.NRGBA{R: 28, G: 32, B: 42, A: 255}
											return label.Layout(gtx)
										}),
										layout.Rigid(func(gtx layout.Context) layout.Dimensions {
											label := material.Caption(ui.th, server.Host)
											label.Color = color.NRGBA{R: 91, G: 99, B: 114, A: 255}
											return label.Layout(gtx)
										}),
									)
								})
							}),
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								btn := material.Button(ui.th, &ui.rowEditClicks[idx], "Edit")
								btn.Background = color.NRGBA{R: 232, G: 236, B: 243, A: 255}
								btn.Color = color.NRGBA{R: 73, G: 81, B: 96, A: 255}
								return btn.Layout(gtx)
							}),
							layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								btn := material.Button(ui.th, &ui.rowDeleteClicks[idx], "x")
								btn.Background = color.NRGBA{R: 232, G: 236, B: 243, A: 255}
								btn.Color = color.NRGBA{R: 73, G: 81, B: 96, A: 255}
								return btn.Layout(gtx)
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
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(sectionTitle(ui.th, "Server")),
				layout.Rigid(layout.Spacer{Height: unit.Dp(12)}.Layout),
				layout.Rigid(ui.field("Name", &ui.name)),
				layout.Rigid(ui.field("Address", &ui.address)),
				layout.Rigid(ui.field("Username", &ui.username)),
				layout.Rigid(ui.field("Password", &ui.password)),
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
		return layout.UniformInset(unit.Dp(14)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			children := []layout.FlexChild{
				layout.Rigid(ui.terminalTabsBar),
				layout.Rigid(layout.Spacer{Height: unit.Dp(10)}.Layout),
			}
			start := 0
			items := []model.Command{}
			if len(ui.terminalTabs) > 0 {
				items = ui.terminalTabs[ui.activeTerminal].Commands
			}
			if len(items) > 12 {
				start = len(items) - 12
			}
			for _, item := range items[start:] {
				entry := item
				children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					line := fmt.Sprintf("$ %s", entry.Command)
					return terminalText(ui.th, line, color.NRGBA{R: 226, G: 232, B: 240, A: 255})(gtx)
				}))
				if entry.Output != "" {
					output := entry.Output
					children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return terminalText(ui.th, output, color.NRGBA{R: 226, G: 232, B: 240, A: 255})(gtx)
					}))
				}
				if entry.Error != "" {
					errText := "ERROR: " + entry.Error
					children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return terminalText(ui.th, errText, color.NRGBA{R: 248, G: 113, B: 113, A: 255})(gtx)
					}))
				}
				children = append(children, layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout))
			}
			children = append(children,
				layout.Flexed(1, layout.Spacer{}.Layout),
				layout.Rigid(layout.Spacer{Height: unit.Dp(10)}.Layout),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					if len(ui.terminalTabs) == 0 {
						return layout.Dimensions{}
					}
					return terminalInput(ui.th, &ui.terminalTabs[ui.activeTerminal].CommandInput, "type command and press Enter")(gtx)
				}),
			)
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
		})
	})
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
	item := model.Command{
		ID:        fmt.Sprintf("cmd-%d", time.Now().UnixNano()),
		Command:   command,
		CreatedAt: time.Now().Format(time.RFC3339),
		UpdatedAt: time.Now().Format(time.RFC3339),
	}
	output, err := opcli.RunCommand("", command)
	item.Output = output
	if err != nil {
		item.Status = model.CommandStatusFailed
		item.Error = err.Error()
		ui.status = err.Error()
	} else {
		item.Status = model.CommandStatusDone
		ui.status = "Command done"
	}
	tab.Commands = append([]model.Command{item}, tab.Commands...)
	tab.CommandInput.SetText("")
}

func (ui *uiState) statusBar(gtx layout.Context) layout.Dimensions {
	return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
		layout.Flexed(1, muted(ui.th, ui.status)),
		layout.Rigid(muted(ui.th, "127.0.0.1:18741")),
	)
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
					return material.Editor(ui.th, editor, "").Layout(gtx)
				}),
			)
		})
	}
}

func (ui *uiState) clearForm() {
	ui.selectedServer = -1
	ui.editing = true
	ui.name.SetText("")
	ui.address.SetText("")
	ui.username.SetText("")
	ui.password.SetText("")
	ui.status = "New server"
}

func (ui *uiState) load(server model.Server) {
	ui.name.SetText(server.Name)
	ui.address.SetText(server.Host)
	ui.password.SetText("")
	ui.username.SetText("")
	for _, credential := range ui.credentials {
		if credential.ID == server.CredentialRef {
			ui.username.SetText(credential.Username)
			break
		}
	}
}

func (ui *uiState) save() {
	name := strings.TrimSpace(ui.name.Text())
	address := strings.TrimSpace(ui.address.Text())
	username := strings.TrimSpace(ui.username.Text())
	password := ui.password.Text()
	if name == "" || address == "" || username == "" {
		ui.status = "Name, address and username are required"
		return
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
	if err := ui.rt.store.UpsertServer(model.Server{
		ID:            serverID,
		Name:          name,
		Host:          address,
		Port:          22,
		Mode:          model.ConnectionModeDirectSSH,
		CredentialRef: credentialID,
	}); err != nil {
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

func panel(gtx layout.Context, w layout.Widget) layout.Dimensions {
	return surface(gtx, color.NRGBA{R: 255, G: 255, B: 255, A: 255}, w)
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
		label.Color = color.NRGBA{R: 18, G: 24, B: 34, A: 255}
		label.Alignment = text.Start
		return label.Layout(gtx)
	}
}

func muted(th *material.Theme, textValue string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		label := material.Caption(th, textValue)
		label.Color = color.NRGBA{R: 94, G: 102, B: 118, A: 255}
		return label.Layout(gtx)
	}
}

func terminalText(th *material.Theme, textValue string, c color.NRGBA) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		label := material.Body2(th, textValue)
		label.Color = c
		return label.Layout(gtx)
	}
}

func terminalInput(th *material.Theme, editor *widget.Editor, hint string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return surface(gtx, color.NRGBA{R: 14, G: 22, B: 34, A: 255}, func(gtx layout.Context) layout.Dimensions {
			return layout.UniformInset(unit.Dp(10)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						label := material.Body1(th, "$")
						label.Color = color.NRGBA{R: 125, G: 211, B: 252, A: 255}
						return label.Layout(gtx)
					}),
					layout.Rigid(layout.Spacer{Width: unit.Dp(10)}.Layout),
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						ed := material.Editor(th, editor, hint)
						ed.Color = color.NRGBA{R: 241, G: 245, B: 249, A: 255}
						ed.HintColor = color.NRGBA{R: 100, G: 116, B: 139, A: 255}
						return ed.Layout(gtx)
					}),
				)
			})
		})
	}
}

func logoMark(th *material.Theme) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		size := gtx.Dp(unit.Dp(34))
		gtx.Constraints.Min.X = size
		gtx.Constraints.Min.Y = size
		gtx.Constraints.Max.X = size
		gtx.Constraints.Max.Y = size
		paint.FillShape(gtx.Ops, color.NRGBA{R: 32, G: 95, B: 168, A: 255}, clip.Rect{Max: gtx.Constraints.Max}.Op())
		return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			label := material.Body1(th, "O")
			label.Color = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
			return label.Layout(gtx)
		})
	}
}

func tabButton(th *material.Theme, click *widget.Clickable, label string, active bool) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		btn := material.Button(th, click, label)
		if active {
			btn.Background = color.NRGBA{R: 36, G: 88, B: 150, A: 255}
		} else {
			btn.Background = color.NRGBA{R: 226, G: 231, B: 238, A: 255}
			btn.Color = color.NRGBA{R: 46, G: 54, B: 67, A: 255}
		}
		return btn.Layout(gtx)
	}
}

func terminalTabButton(th *material.Theme, click *widget.Clickable, label string, active bool) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		btn := material.Button(th, click, label)
		if active {
			btn.Background = color.NRGBA{R: 30, G: 41, B: 59, A: 255}
			btn.Color = color.NRGBA{R: 226, G: 232, B: 240, A: 255}
		} else {
			btn.Background = color.NRGBA{R: 15, G: 23, B: 42, A: 255}
			btn.Color = color.NRGBA{R: 148, G: 163, B: 184, A: 255}
		}
		return btn.Layout(gtx)
	}
}
