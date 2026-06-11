package logs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/tao-vin/opsera/internal/model"
)

type Store struct {
	path string
	mu   sync.Mutex
}

func NewStore(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &Store{path: filepath.Join(root, "ops.log")}, nil
}

func (s *Store) Append(level model.LogLevel, source, message, serverID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry := model.LogEntry{
		ID:        time.Now().Format("20060102150405.000000000"),
		Timestamp: time.Now().Format(time.RFC3339),
		Level:     level,
		Source:    source,
		Message:   message,
		ServerID:  serverID,
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(raw, '\n'))
	return err
}
