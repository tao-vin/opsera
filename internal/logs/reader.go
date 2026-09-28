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

	entries := make([]model.LogEntry, 0, limit)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var entry model.LogEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err == nil {
			if len(entries) == limit {
				copy(entries, entries[1:])
				entries[len(entries)-1] = entry
			} else {
				entries = append(entries, entry)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}
