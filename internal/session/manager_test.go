package session

import (
	"path/filepath"
	"testing"

	"github.com/tao-vin/opsera/internal/config"
	"github.com/tao-vin/opsera/internal/logs"
)

func TestManagerStartDeduplicatesTarget(t *testing.T) {
	root := t.TempDir()
	store, err := config.NewStore(filepath.Join(root, "config"))
	if err != nil {
		t.Fatal(err)
	}
	logStore, err := logs.NewStore(filepath.Join(root, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(store, logStore)
	first := manager.Start("192.168.70.10", "argv")
	second := manager.Start("192.168.70.10", "forwarded")

	items := manager.List()
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, items=%#v", len(items), items)
	}
	if items[0].ID != first.ID || second.ID != first.ID {
		t.Fatalf("session ID should be reused: first=%q second=%q items=%#v", first.ID, second.ID, items)
	}
	if items[0].Source != "forwarded" {
		t.Fatalf("source = %q", items[0].Source)
	}
}
