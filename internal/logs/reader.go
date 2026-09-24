package logs

import (
	"bufio"
	"encoding/json"
	"os"

	"github.com/tao-vin/opsera/internal/model"
)

func (s *Store) ReadLatest(limit int) ([]model.LogEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return []model.LogEntry{}, nil
		}
		return nil, err
	}
	defer f.Close()

	var entries []model.LogEntry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var entry model.LogEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err == nil {
			entries = append(entries, entry)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(entries) <= limit {
		return entries, nil
	}
	return entries[len(entries)-limit:], nil
}
