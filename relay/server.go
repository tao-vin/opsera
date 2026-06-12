package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

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
	CreatedAt      time.Time `json:"createdAt"`
	LastSeenAt     time.Time `json:"lastSeenAt"`
}

type store struct {
	path    string
	mu      sync.RWMutex
	devices map[string]device
}

type server struct {
	store *store
}

func main() {
	addr := env("OPSERA_RELAY_ADDR", ":18742")
	dataDir := env("OPSERA_RELAY_DATA", filepath.Join(".", "relay-data"))
	st, err := newStore(filepath.Join(dataDir, "devices.json"))
	if err != nil {
		log.Fatal(err)
	}
	s := &server{store: st}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/devices/register", s.handleRegister)
	mux.HandleFunc("/devices/heartbeat", s.handleHeartbeat)
	mux.HandleFunc("/devices/verify", s.handleVerify)
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
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	var req struct {
		Name       string `json:"name"`
		Secret     string `json:"secret"`
		AccessCode string `json:"accessCode"`
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
	d := device{
		Code:           s.newUniqueMachineCode(),
		Name:           req.Name,
		SecretHash:     secretHash,
		AccessCodeHash: accessCodeHash,
		CreatedAt:      now,
		LastSeenAt:     now,
	}
	if err := s.store.put(d); err != nil {
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
