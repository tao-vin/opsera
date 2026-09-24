package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tao-vin/opsera/internal/relayws"
	"github.com/tao-vin/opsera/internal/version"
	"golang.org/x/crypto/pbkdf2"
)

const (
	hashIterations = 210000
	maxJSONBody    = 64 << 10
)

type device struct {
	Code           string    `json:"code"`
	Name           string    `json:"name"`
	SecretHash     string    `json:"secretHash"`
	AccessCodeHash string    `json:"accessCodeHash"`
	Version        string    `json:"version,omitempty"`
	BuildTime      string    `json:"buildTime,omitempty"`
	Commit         string    `json:"commit,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
	LastSeenAt     time.Time `json:"lastSeenAt"`
}

type store struct {
	path    string
	mu      sync.RWMutex
	devices map[string]device
}

type server struct {
	store   *store
	agents  *agentHub
	files   *fileHub
	dataDir string
}

type fileManifest struct {
	FileID    string    `json:"fileId"`
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	PartSize  int64     `json:"partSize"`
	PartCount int       `json:"partCount"`
	CreatedAt time.Time `json:"createdAt"`
}

type agentHub struct {
	mu     sync.RWMutex
	agents map[string]*agentConn
}

type agentConn struct {
	code    string
	conn    *relayws.Conn
	send    chan relayEnvelope
	pending map[string]chan relayws.Message
	mu      sync.Mutex
	writeMu sync.Mutex
}

type relayEnvelope struct {
	To      string          `json:"to,omitempty"`
	Message relayws.Message `json:"message"`
}

type fileHub struct {
	mu    sync.Mutex
	pairs map[string]*filePair
}

type filePair struct {
	agent   *fileSession
	control *fileSession
	started bool
}

type fileSession struct {
	conn *relayws.Conn
	done chan struct{}
}

func main() {
	addr := env("OPSERA_RELAY_ADDR", ":18742")
	dataDir := env("OPSERA_RELAY_DATA", filepath.Join(".", "relay-data"))
	st, err := newStore(filepath.Join(dataDir, "devices.json"))
	if err != nil {
		log.Fatal(err)
	}
	s := &server{store: st, agents: &agentHub{agents: map[string]*agentConn{}}, files: &fileHub{pairs: map[string]*filePair{}}, dataDir: dataDir}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/devices/register", s.handleRegister)
	mux.HandleFunc("/devices/heartbeat", s.handleHeartbeat)
	mux.HandleFunc("/devices/verify", s.handleVerify)
	mux.HandleFunc("/agent/ws", s.handleAgentWS)
	mux.HandleFunc("/control/ws", s.handleControlWS)
	mux.HandleFunc("/agent/file/ws", s.handleAgentFileWS)
	mux.HandleFunc("/control/file/ws", s.handleControlFileWS)
	mux.HandleFunc("/files/init", s.handleFileInit)
	mux.HandleFunc("/files/", s.handleFiles)
	mux.HandleFunc("/devices/", s.handleDevice)
	log.Printf("opsera relay listening on %s", addr)
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           securityHeaders(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	log.Fatal(httpServer.ListenAndServe())
}

func (s *server) handleAgentWS(w http.ResponseWriter, r *http.Request) {
	code := normalizeCode(r.URL.Query().Get("machineCode"))
	secret := r.URL.Query().Get("secret")
	d, err := s.authenticateDevice(code, secret)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	conn, err := relayws.Upgrade(w, r)
	if err != nil {
		log.Printf("agent websocket upgrade failed: %v", err)
		return
	}
	agent := &agentConn{
		code:    d.Code,
		conn:    conn,
		send:    make(chan relayEnvelope, 32),
		pending: map[string]chan relayws.Message{},
	}
	s.agents.put(agent)
	defer func() {
		s.agents.remove(d.Code, agent)
		_ = conn.Close()
	}()
	go agent.writeLoop()
	for {
		var msg relayws.Message
		if err := conn.ReadJSON(&msg); err != nil {
			if !errors.Is(err, io.EOF) {
				log.Printf("agent read failed %s: %v", d.Code, err)
			}
			return
		}
		agent.resolve(msg)
	}
}

func (s *server) handleControlWS(w http.ResponseWriter, r *http.Request) {
	code := normalizeCode(r.URL.Query().Get("machineCode"))
	accessCode := r.URL.Query().Get("accessCode")
	d, ok := s.store.get(code)
	if !ok || !verifySecret(d.AccessCodeHash, accessCode) {
		http.Error(w, "invalid machine code or access code", http.StatusUnauthorized)
		return
	}
	conn, err := relayws.Upgrade(w, r)
	if err != nil {
		log.Printf("control websocket upgrade failed: %v", err)
		return
	}
	defer conn.Close()
	for {
		var msg relayws.Message
		if err := conn.ReadJSON(&msg); err != nil {
			return
		}
		agent := s.agents.get(d.Code)
		if agent == nil {
			_ = conn.WriteJSON(relayws.Message{Type: "result", ID: msg.ID, Error: "agent offline"})
			continue
		}
		if msg.Type == "file-upload" {
			if err := agent.dispatch(msg); err != nil {
				_ = conn.WriteJSON(relayws.Message{Type: "result", ID: msg.ID, Error: err.Error()})
				continue
			}
			_ = conn.WriteJSON(relayws.Message{Type: "accepted", ID: msg.ID})
			continue
		}
		result, err := agent.request(msg, 10*time.Minute)
		if err != nil {
			_ = conn.WriteJSON(relayws.Message{Type: "result", ID: msg.ID, Error: err.Error()})
			continue
		}
		_ = conn.WriteJSON(result)
	}
}

func (a *agentConn) dispatch(msg relayws.Message) error {
	if strings.TrimSpace(msg.ID) == "" {
		msg.ID = fmt.Sprintf("task-%d", time.Now().UnixNano())
	}
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	if err := a.conn.WriteJSON(msg); err != nil {
		_ = a.conn.Close()
		return err
	}
	return nil
}

func (s *server) handleAgentFileWS(w http.ResponseWriter, r *http.Request) {
	code := normalizeCode(r.URL.Query().Get("machineCode"))
	secret := r.URL.Query().Get("secret")
	taskID := strings.TrimSpace(r.URL.Query().Get("taskId"))
	if taskID == "" {
		http.Error(w, "taskId is required", http.StatusBadRequest)
		return
	}
	d, err := s.authenticateDevice(code, secret)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	conn, err := relayws.Upgrade(w, r)
	if err != nil {
		log.Printf("agent file websocket upgrade failed: %v", err)
		return
	}
	log.Printf("file websocket joined side=agent code=%s task=%s", d.Code, taskID)
	s.files.join(d.Code, taskID, true, conn)
}

func (s *server) handleControlFileWS(w http.ResponseWriter, r *http.Request) {
	code := normalizeCode(r.URL.Query().Get("machineCode"))
	accessCode := r.URL.Query().Get("accessCode")
	taskID := strings.TrimSpace(r.URL.Query().Get("taskId"))
	if taskID == "" {
		http.Error(w, "taskId is required", http.StatusBadRequest)
		return
	}
	d, ok := s.store.get(code)
	if !ok || !verifySecret(d.AccessCodeHash, accessCode) {
		http.Error(w, "invalid machine code or access code", http.StatusUnauthorized)
		return
	}
	conn, err := relayws.Upgrade(w, r)
	if err != nil {
		log.Printf("control file websocket upgrade failed: %v", err)
		return
	}
	log.Printf("file websocket joined side=control code=%s task=%s", d.Code, taskID)
	s.files.join(d.Code, taskID, false, conn)
}

func (h *fileHub) join(code, taskID string, agentSide bool, conn *relayws.Conn) {
	key := code + ":" + taskID
	session := &fileSession{conn: conn, done: make(chan struct{})}
	h.mu.Lock()
	pair := h.pairs[key]
	if pair == nil {
		pair = &filePair{}
		h.pairs[key] = pair
	}
	if agentSide {
		if pair.agent != nil {
			_ = pair.agent.conn.Close()
		}
		pair.agent = session
	} else {
		if pair.control != nil {
			_ = pair.control.conn.Close()
		}
		pair.control = session
	}
	if pair.agent != nil && pair.control != nil && !pair.started {
		pair.started = true
		log.Printf("file websocket paired key=%s", key)
		go h.pipe(key, pair.agent, pair.control)
	}
	h.mu.Unlock()
	<-session.done
}

func (h *fileHub) pipe(key string, agent, control *fileSession) {
	var once sync.Once
	closeBoth := func() {
		log.Printf("file websocket closed key=%s", key)
		_ = agent.conn.Close()
		_ = control.conn.Close()
		close(agent.done)
		close(control.done)
		h.mu.Lock()
		delete(h.pairs, key)
		h.mu.Unlock()
	}
	forward := func(name string, src, dst *fileSession) {
		defer once.Do(closeBoth)
		seen := 0
		for {
			opcode, payload, err := src.conn.ReadMessage()
			if err != nil {
				log.Printf("file websocket read stopped key=%s side=%s frames=%d err=%v", key, name, seen, err)
				return
			}
			seen++
			if seen <= 3 || seen%32 == 0 {
				log.Printf("file websocket frame key=%s side=%s opcode=%d bytes=%d frames=%d", key, name, opcode, len(payload), seen)
			}
			if err := dst.conn.WriteMessage(opcode, payload); err != nil {
				log.Printf("file websocket write stopped key=%s side=%s frames=%d err=%v", key, name, seen, err)
				return
			}
		}
	}
	go forward("agent-to-control", agent, control)
	forward("control-to-agent", control, agent)
}

func (h *agentHub) put(agent *agentConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if old := h.agents[agent.code]; old != nil {
		_ = old.conn.Close()
	}
	h.agents[agent.code] = agent
}

func (h *agentHub) get(code string) *agentConn {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.agents[code]
}

func (h *agentHub) remove(code string, agent *agentConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.agents[code] == agent {
		delete(h.agents, code)
	}
}

func (a *agentConn) request(msg relayws.Message, timeout time.Duration) (relayws.Message, error) {
	if strings.TrimSpace(msg.ID) == "" {
		msg.ID = fmt.Sprintf("task-%d", time.Now().UnixNano())
	}
	ch := make(chan relayws.Message, 1)
	a.mu.Lock()
	a.pending[msg.ID] = ch
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.pending, msg.ID)
		a.mu.Unlock()
	}()
	select {
	case a.send <- relayEnvelope{Message: msg}:
	case <-time.After(5 * time.Second):
		return relayws.Message{}, fmt.Errorf("agent send timeout")
	}
	select {
	case result := <-ch:
		return result, nil
	case <-time.After(timeout):
		return relayws.Message{}, fmt.Errorf("agent result timeout")
	}
}

func (a *agentConn) resolve(msg relayws.Message) {
	a.mu.Lock()
	ch := a.pending[msg.ID]
	a.mu.Unlock()
	if ch != nil {
		ch <- msg
	}
}

func (a *agentConn) writeLoop() {
	for item := range a.send {
		a.writeMu.Lock()
		err := a.conn.WriteJSON(item.Message)
		a.writeMu.Unlock()
		if err != nil {
			_ = a.conn.Close()
			return
		}
	}
}

func newStore(path string) (*store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	st := &store{path: path, devices: map[string]device{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, st.saveLocked()
	}
	if err != nil {
		return nil, err
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &st.devices); err != nil {
			return nil, err
		}
	}
	return st, nil
}

func (s *store) saveLocked() error {
	raw, err := json.MarshalIndent(s.devices, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, raw, 0o600)
}

func (s *store) put(d device) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.devices[d.Code]; exists {
		return fmt.Errorf("device already exists")
	}
	s.devices[d.Code] = d
	return s.saveLocked()
}

func (s *store) get(code string) (device, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.devices[code]
	return d, ok
}

func (s *store) update(code string, fn func(device) device) (device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[code]
	if !ok {
		return device{}, fmt.Errorf("device not found")
	}
	d = fn(d)
	s.devices[code] = d
	return d, s.saveLocked()
}

func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	info := version.Current()
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": info})
}

func (s *server) handleFileInit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req fileManifest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.FileID = normalizeFileID(req.FileID)
	if req.FileID == "" {
		req.FileID = newFileID()
	}
	if req.PartSize <= 0 || req.PartSize > 32<<20 {
		http.Error(w, "invalid partSize", http.StatusBadRequest)
		return
	}
	if req.Size < 0 || req.PartCount <= 0 || req.SHA256 == "" {
		http.Error(w, "size, sha256 and partCount are required", http.StatusBadRequest)
		return
	}
	req.CreatedAt = time.Now().UTC()
	dir := s.fileDir(req.FileID)
	if err := os.MkdirAll(filepath.Join(dir, "parts"), 0o700); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	raw, err := json.MarshalIndent(req, "", "  ")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0o600); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, req)
}

func (s *server) handleFiles(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/files/")
	parts := strings.Split(rest, "/")
	if len(parts) < 2 {
		http.Error(w, "invalid file path", http.StatusBadRequest)
		return
	}
	fileID := normalizeFileID(parts[0])
	if fileID == "" {
		http.Error(w, "invalid file id", http.StatusBadRequest)
		return
	}
	switch {
	case len(parts) == 2 && parts[1] == "manifest" && r.Method == http.MethodGet:
		http.ServeFile(w, r, filepath.Join(s.fileDir(fileID), "manifest.json"))
	case len(parts) == 3 && parts[1] == "parts":
		index, err := strconv.Atoi(parts[2])
		if err != nil || index < 0 {
			http.Error(w, "invalid part index", http.StatusBadRequest)
			return
		}
		partPath := filepath.Join(s.fileDir(fileID), "parts", fmt.Sprintf("%06d.part", index))
		if r.Method == http.MethodPut {
			if err := os.MkdirAll(filepath.Dir(partPath), 0o700); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			tmp := partPath + ".tmp"
			out, err := os.Create(tmp)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			_, copyErr := io.Copy(out, http.MaxBytesReader(w, r.Body, 64<<20))
			closeErr := out.Close()
			if copyErr != nil {
				_ = os.Remove(tmp)
				http.Error(w, copyErr.Error(), http.StatusBadRequest)
				return
			}
			if closeErr != nil {
				_ = os.Remove(tmp)
				http.Error(w, closeErr.Error(), http.StatusInternalServerError)
				return
			}
			if err := os.Rename(tmp, partPath); err != nil {
				_ = os.Remove(tmp)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "part": index})
			return
		}
		if r.Method == http.MethodGet {
			http.ServeFile(w, r, partPath)
			return
		}
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (s *server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	var req struct {
		MachineCode string `json:"machineCode"`
		Name        string `json:"name"`
		Secret      string `json:"secret"`
		AccessCode  string `json:"accessCode"`
		Version     string `json:"version"`
		BuildTime   string `json:"buildTime"`
		Commit      string `json:"commit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || req.Secret == "" || req.AccessCode == "" {
		http.Error(w, "name, secret and accessCode are required", http.StatusBadRequest)
		return
	}
	if len(req.Secret) < 24 || len(req.AccessCode) < 6 {
		http.Error(w, "secret or accessCode is too weak", http.StatusBadRequest)
		return
	}
	secretHash, err := hashSecret(req.Secret)
	if err != nil {
		http.Error(w, "could not hash secret", http.StatusInternalServerError)
		return
	}
	accessCodeHash, err := hashSecret(req.AccessCode)
	if err != nil {
		http.Error(w, "could not hash access code", http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC()
	code := normalizeCode(req.MachineCode)
	if code == "" {
		code = s.newUniqueMachineCode()
	}
	d := device{
		Code:           code,
		Name:           req.Name,
		SecretHash:     secretHash,
		AccessCodeHash: accessCodeHash,
		Version:        strings.TrimSpace(req.Version),
		BuildTime:      strings.TrimSpace(req.BuildTime),
		Commit:         strings.TrimSpace(req.Commit),
		CreatedAt:      now,
		LastSeenAt:     now,
	}
	if existing, ok := s.store.get(code); ok {
		d.CreatedAt = existing.CreatedAt
		_, err = s.store.update(code, func(device) device { return d })
	} else {
		err = s.store.put(d)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"machineCode": d.Code})
}

func (s *server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	var req struct {
		MachineCode string `json:"machineCode"`
		Secret      string `json:"secret"`
		Version     string `json:"version"`
		BuildTime   string `json:"buildTime"`
		Commit      string `json:"commit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	d, err := s.authenticateDevice(req.MachineCode, req.Secret)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	d, err = s.store.update(d.Code, func(current device) device {
		current.LastSeenAt = time.Now().UTC()
		if strings.TrimSpace(req.Version) != "" {
			current.Version = strings.TrimSpace(req.Version)
		}
		if strings.TrimSpace(req.BuildTime) != "" {
			current.BuildTime = strings.TrimSpace(req.BuildTime)
		}
		if strings.TrimSpace(req.Commit) != "" {
			current.Commit = strings.TrimSpace(req.Commit)
		}
		return current
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, publicDevice(d))
}

func (s *server) handleVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	var req struct {
		MachineCode string `json:"machineCode"`
		AccessCode  string `json:"accessCode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	d, ok := s.store.get(normalizeCode(req.MachineCode))
	if !ok || !verifySecret(d.AccessCodeHash, req.AccessCode) {
		http.Error(w, "invalid machine code or access code", http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, publicDevice(d))
}

func (s *server) handleDevice(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	code := strings.TrimPrefix(r.URL.Path, "/devices/")
	d, ok := s.store.get(normalizeCode(code))
	if !ok {
		http.Error(w, "device not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, publicDevice(d))
}

func (s *server) fileDir(fileID string) string {
	return filepath.Join(s.dataDir, "files", normalizeFileID(fileID))
}

func normalizeFileID(value string) string {
	value = strings.TrimSpace(value)
	var b strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func newFileID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func (s *server) authenticateDevice(code string, secret string) (device, error) {
	d, ok := s.store.get(normalizeCode(code))
	if !ok || !verifySecret(d.SecretHash, secret) {
		return device{}, fmt.Errorf("invalid machine code or secret")
	}
	return d, nil
}

func publicDevice(d device) map[string]any {
	return map[string]any{
		"machineCode": d.Code,
		"name":        d.Name,
		"online":      time.Since(d.LastSeenAt) < 90*time.Second,
		"lastSeenAt":  d.LastSeenAt,
		"version":     d.Version,
		"buildTime":   d.BuildTime,
		"commit":      d.Commit,
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (s *server) newUniqueMachineCode() string {
	for i := 0; i < 16; i++ {
		code := newMachineCode()
		if _, exists := s.store.get(code); !exists {
			return code
		}
	}
	panic("could not allocate machine code")
}

func newMachineCode() string {
	var b [6]byte
	for {
		if _, err := rand.Read(b[:]); err != nil {
			panic(err)
		}
		n := int(b[0])<<16 | int(b[1])<<8 | int(b[2])
		code := fmt.Sprintf("%09d", n%1000000000)
		if code != "000000000" {
			return code
		}
	}
}

func normalizeCode(code string) string {
	code = strings.TrimSpace(code)
	code = strings.ReplaceAll(code, "-", "")
	code = strings.ReplaceAll(code, " ", "")
	return code
}

func hashSecret(value string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	derived := pbkdf2.Key([]byte(value), salt, hashIterations, 32, sha256.New)
	return fmt.Sprintf("pbkdf2$%d$%s$%s", hashIterations, hex.EncodeToString(salt), hex.EncodeToString(derived)), nil
}

func verifySecret(encoded string, value string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2" {
		return false
	}
	var iterations int
	if _, err := fmt.Sscanf(parts[1], "%d", &iterations); err != nil || iterations < 100000 {
		return false
	}
	salt, err := hex.DecodeString(parts[2])
	if err != nil {
		return false
	}
	expected, err := hex.DecodeString(parts[3])
	if err != nil {
		return false
	}
	actual := pbkdf2.Key([]byte(value), salt, iterations, len(expected), sha256.New)
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func env(key string, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}
