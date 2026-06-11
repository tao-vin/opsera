package session

import (
	"fmt"
	"sync"
	"time"

	"github.com/tao-vin/opsera/internal/logs"
	"github.com/tao-vin/opsera/internal/model"
)

type CommandQueue struct {
	logs  *logs.Store
	mu    sync.RWMutex
	items []model.Command
}

func NewCommandQueue(logStore *logs.Store) *CommandQueue {
	return &CommandQueue{logs: logStore}
}

func (q *CommandQueue) Add(current []model.Session, command string) model.Command {
	item := model.Command{
		ID:        fmt.Sprintf("cmd-%d", time.Now().UnixNano()),
		Command:   command,
		Status:    model.CommandStatusQueued,
		CreatedAt: time.Now().Format(time.RFC3339),
	}
	if len(current) > 0 {
		item.SessionID = current[0].ID
		item.Target = current[0].Target
	}
	q.mu.Lock()
	q.items = append([]model.Command{item}, q.items...)
	if len(q.items) > 100 {
		q.items = q.items[:100]
	}
	q.mu.Unlock()
	_ = q.logs.Append(model.LogLevelInfo, "command", "queued: "+command, item.SessionID)
	return item
}

func (q *CommandQueue) List() []model.Command {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return append([]model.Command{}, q.items...)
}
