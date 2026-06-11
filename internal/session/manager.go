package session

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/tao-vin/opsera/internal/config"
	"github.com/tao-vin/opsera/internal/logs"
	"github.com/tao-vin/opsera/internal/model"
)

type Manager struct {
	store *config.Store
	logs  *logs.Store
	mu    sync.RWMutex
	items []model.Session
}

func NewManager(store *config.Store, logStore *logs.Store) *Manager {
	return &Manager{store: store, logs: logStore}
}

func (m *Manager) List() []model.Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]model.Session{}, m.items...)
}

func (m *Manager) Start(target, source string) model.Session {
	target = strings.TrimSpace(target)
	if target == "" {
		target = "manual"
	}
	server, ok := m.matchServer(target)
	now := time.Now().Format(time.RFC3339)
	item := model.Session{
		ID:        fmt.Sprintf("ses-%d", time.Now().UnixNano()),
		Target:    target,
		Source:    source,
		StartedAt: now,
	}
	if source != "" && strings.Contains(source, "|") {
		parts := strings.SplitN(source, "|", 2)
		item.Source = parts[0]
		item.LaunchArg = parts[1]
	}
	if ok {
		item.ServerID = server.ID
		item.Server = server.Name
		item.Status = model.SessionStatusReady
		item.Message = "matched server"
		_ = m.logs.Append(model.LogLevelInfo, "session", "session ready: "+server.Name, server.ID)
	} else {
		item.Status = model.SessionStatusCaptured
		item.Message = "vpn target captured"
		_ = m.logs.Append(model.LogLevelInfo, "session", "vpn target captured: "+target, "")
	}
	m.mu.Lock()
	m.items = append([]model.Session{item}, m.items...)
	if len(m.items) > 100 {
		m.items = m.items[:100]
	}
	m.mu.Unlock()
	return item
}

func (m *Manager) matchServer(target string) (model.Server, bool) {
	q := strings.ToLower(strings.TrimSpace(target))
	for _, server := range m.store.Snapshot().Servers {
		if strings.ToLower(server.ID) == q ||
			strings.ToLower(server.Name) == q ||
			strings.ToLower(server.Host) == q ||
			strings.Contains(strings.ToLower(server.Name), q) ||
			strings.Contains(strings.ToLower(server.Host), q) {
			return server, true
		}
	}
	return model.Server{}, false
}
