package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/tao-vin/opsera/internal/model"
)

type State struct {
	Servers     []model.Server     `json:"servers"`
	Credentials []model.Credential `json:"credentials"`
}

type Store struct {
	path string
	mu   sync.RWMutex
	data State
}

func NewStore(root string) (*Store, error) {
	if root == "" {
		return nil, errors.New("config root is required")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	store := &Store{
		path: filepath.Join(root, "state.json"),
		data: State{
			Servers:     []model.Server{},
			Credentials: []model.Credential{},
		},
	}
	if err := store.load(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Store) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s.saveLocked()
	}
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		s.normalizeLocked()
		return nil
	}
	if err := json.Unmarshal(raw, &s.data); err != nil {
		return err
	}
	s.normalizeLocked()
	return nil
}

func (s *Store) saveLocked() error {
	s.normalizeLocked()
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, raw, 0o600)
}

func (s *Store) normalizeLocked() {
	if s.data.Servers == nil {
		s.data.Servers = []model.Server{}
	}
	if s.data.Credentials == nil {
		s.data.Credentials = []model.Credential{}
	}
}

func (s *Store) Snapshot() State {
	s.mu.RLock()
	defer s.mu.RUnlock()

	servers := append([]model.Server{}, s.data.Servers...)
	credentials := append([]model.Credential{}, s.data.Credentials...)
	return State{
		Servers:     servers,
		Credentials: credentials,
	}
}

func (s *Store) FindServers(query string) []model.Server {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if strings.TrimSpace(query) == "" {
		return append([]model.Server(nil), s.data.Servers...)
	}
	q := strings.ToLower(strings.TrimSpace(query))
	var result []model.Server
	for _, server := range s.data.Servers {
		if strings.Contains(strings.ToLower(server.Name), q) || strings.Contains(strings.ToLower(server.Host), q) {
			result = append(result, server)
		}
	}
	return result
}

func (s *Store) UpsertServer(server model.Server) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Servers {
		if s.data.Servers[i].ID == server.ID {
			s.data.Servers[i] = server
			return s.saveLocked()
		}
	}
	s.data.Servers = append(s.data.Servers, server)
	return s.saveLocked()
}

func (s *Store) DeleteServer(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	filtered := s.data.Servers[:0]
	for _, item := range s.data.Servers {
		if item.ID != id {
			filtered = append(filtered, item)
		}
	}
	s.data.Servers = filtered
	return s.saveLocked()
}

func (s *Store) UpsertCredential(credential model.Credential) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Credentials {
		if s.data.Credentials[i].ID == credential.ID {
			s.data.Credentials[i] = credential
			return s.saveLocked()
		}
	}
	s.data.Credentials = append(s.data.Credentials, credential)
	return s.saveLocked()
}

func (s *Store) DeleteCredential(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	filtered := s.data.Credentials[:0]
	for _, item := range s.data.Credentials {
		if item.ID != id {
			filtered = append(filtered, item)
		}
	}
	s.data.Credentials = filtered
	return s.saveLocked()
}
