package events

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

type Event struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	TabID     string `json:"tabId,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
	Command   string `json:"command,omitempty"`
	Output    string `json:"output,omitempty"`
	Error     string `json:"error,omitempty"`
	Status    string `json:"status,omitempty"`
	CreatedAt string `json:"createdAt"`
}

func Write(root string, event Event) error {
	dir := filepath.Join(root, "events")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if event.ID == "" {
		event.ID = "evt-" + time.Now().Format("20060102150405.000000000")
	}
	if event.CreatedAt == "" {
		event.CreatedAt = time.Now().Format(time.RFC3339)
	}
	raw, err := json.MarshalIndent(event, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, event.ID+".json"), raw, 0o600)
}

func ReadAll(root string) ([]Event, error) {
	dir := filepath.Join(root, "events")
	items, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return []Event{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Event{}
	for _, item := range items {
		if item.IsDir() || filepath.Ext(item.Name()) != ".json" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, item.Name()))
		if err != nil {
			continue
		}
		var event Event
		if err := json.Unmarshal(raw, &event); err == nil {
			out = append(out, event)
		}
	}
	return out, nil
}
