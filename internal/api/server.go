package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/tao-vin/opsera/internal/config"
	"github.com/tao-vin/opsera/internal/crypto"
	"github.com/tao-vin/opsera/internal/logs"
	"github.com/tao-vin/opsera/internal/model"
	"github.com/tao-vin/opsera/internal/session"
)

type Server struct {
	store    *config.Store
	logs     *logs.Store
	vault    *crypto.Vault
	sessions *session.Manager
	commands *session.CommandQueue
	onLaunch func([]string)
}

func New(store *config.Store, logStore *logs.Store, vault *crypto.Vault, sessions *session.Manager, commands *session.CommandQueue, onLaunch func([]string)) *Server {
	return &Server{store: store, logs: logStore, vault: vault, sessions: sessions, commands: commands, onLaunch: onLaunch}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/servers", s.handleServers)
	mux.HandleFunc("/credentials", s.handleCredentials)
	mux.HandleFunc("/sessions", s.handleSessions)
	mux.HandleFunc("/launch", s.handleLaunch)
	mux.HandleFunc("/session/command", s.handleSessionCommand)
	mux.HandleFunc("/session/commands", s.handleSessionCommands)
	mux.HandleFunc("/logs", s.handleLogs)
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) handleServers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		query := r.URL.Query().Get("q")
		if strings.TrimSpace(query) != "" {
			_ = json.NewEncoder(w).Encode(s.store.FindServers(query))
			return
		}
		_ = json.NewEncoder(w).Encode(s.store.Snapshot().Servers)
	case http.MethodPost:
		var server model.Server
		if err := json.NewDecoder(r.Body).Decode(&server); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.store.UpsertServer(server); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_ = s.logs.Append(model.LogLevelInfo, "api", "server upserted", server.ID)
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if strings.TrimSpace(id) == "" {
			http.Error(w, "id is required", http.StatusBadRequest)
			return
		}
		if err := s.store.DeleteServer(id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_ = s.logs.Append(model.LogLevelWarn, "api", "server deleted", id)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleCredentials(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		items := s.store.Snapshot().Credentials
		out := make([]model.Credential, 0, len(items))
		for _, item := range items {
			out = append(out, model.Credential{
				ID:       item.ID,
				Name:     item.Name,
				Username: item.Username,
			})
		}
		_ = json.NewEncoder(w).Encode(out)
	case http.MethodPost:
		var req struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Username string `json:"username"`
			Secret   string `json:"secret"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(req.Secret) == "" {
			http.Error(w, "secret is required", http.StatusBadRequest)
			return
		}
		cipherText, err := s.vault.Encrypt(req.Secret)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		credential := model.Credential{
			ID:           req.ID,
			Name:         req.Name,
			Username:     req.Username,
			SecretCipher: cipherText,
		}
		if err := s.store.UpsertCredential(credential); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_ = s.logs.Append(model.LogLevelInfo, "api", "credential upserted", credential.ID)
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if strings.TrimSpace(id) == "" {
			http.Error(w, "id is required", http.StatusBadRequest)
			return
		}
		if err := s.store.DeleteCredential(id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_ = s.logs.Append(model.LogLevelWarn, "api", "credential deleted", id)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleLogs(w http.ResponseWriter, _ *http.Request) {
	items, err := s.logs.ReadLatest(200)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = json.NewEncoder(w).Encode(items)
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		_ = json.NewEncoder(w).Encode(s.sessions.List())
	case http.MethodPost:
		var req struct {
			Target string `json:"target"`
			Source string `json:"source"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		item := s.sessions.Start(req.Target, req.Source)
		_ = json.NewEncoder(w).Encode(item)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleLaunch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Args []string `json:"args"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	raw, _ := json.Marshal(req.Args)
	_ = s.logs.Append(model.LogLevelInfo, "argv", string(raw), "")
	item := s.sessions.Start(session.LaunchTarget(req.Args), "forwarded")
	if s.onLaunch != nil {
		s.onLaunch(req.Args)
	}
	_ = json.NewEncoder(w).Encode(item)
}

func (s *Server) handleSessionCommand(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Command string `json:"command"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.Command = strings.TrimSpace(req.Command)
	if req.Command == "" {
		http.Error(w, "command is required", http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(s.commands.Add(s.sessions.List(), req.Command))
}

func (s *Server) handleSessionCommands(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	_ = json.NewEncoder(w).Encode(s.commands.List())
}
