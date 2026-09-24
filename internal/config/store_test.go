package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStoreLoadsUTF8BOMAndSearchesServerNotes(t *testing.T) {
	root := t.TempDir()
	statePath := filepath.Join(root, "state.json")
	raw := []byte("\xEF\xBB\xBF" + `{
  "servers": [
    {
      "id": "srv-fei",
      "name": "fei",
      "host": "8.212.146.171",
      "port": 22222,
      "mode": "direct_ssh",
      "credentialRef": "cred-fei",
      "notes": "opsera relay domain opsera.deepcode.art"
    }
  ],
  "credentials": []
}`)
	if err := os.WriteFile(statePath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	got := store.FindServers("deepcode.art")
	if len(got) != 1 {
		t.Fatalf("FindServers matched %d servers, want 1", len(got))
	}
	if got[0].ID != "srv-fei" {
		t.Fatalf("matched server ID = %q, want srv-fei", got[0].ID)
	}
}
