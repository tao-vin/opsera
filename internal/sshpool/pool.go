package sshpool

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/tao-vin/opsera/internal/config"
	"github.com/tao-vin/opsera/internal/crypto"
	"github.com/tao-vin/opsera/internal/logs"
	"github.com/tao-vin/opsera/internal/model"
)

type Pool struct {
	store *config.Store
	vault *crypto.Vault
	logs  *logs.Store

	mu      sync.Mutex
	clients map[string]*ssh.Client
}

func New(store *config.Store, vault *crypto.Vault, logStore *logs.Store) *Pool {
	return &Pool{
		store:   store,
		vault:   vault,
		logs:    logStore,
		clients: map[string]*ssh.Client{},
	}
}

func (p *Pool) Run(serverRef string, command string) (string, error) {
	server, credential, password, err := p.resolve(serverRef)
	if err != nil {
		return "", err
	}
	client, err := p.client(server, credential, password)
	if err != nil {
		return "", err
	}
	session, err := client.NewSession()
	if err != nil {
		p.drop(server.ID)
		client, err = p.client(server, credential, password)
		if err != nil {
			return "", err
		}
		session, err = client.NewSession()
		if err != nil {
			return "", err
		}
	}
	defer session.Close()
	out, err := session.CombinedOutput(command)
	return string(out), err
}

func (p *Pool) CloseAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, client := range p.clients {
		_ = client.Close()
		delete(p.clients, id)
	}
}

func (p *Pool) client(server model.Server, credential model.Credential, password string) (*ssh.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if client := p.clients[server.ID]; client != nil && alive(client) {
		return client, nil
	}
	if old := p.clients[server.ID]; old != nil {
		_ = old.Close()
		delete(p.clients, server.ID)
	}
	port := server.Port
	if port <= 0 {
		port = 22
	}
	cfg := &ssh.ClientConfig{
		User:            credential.Username,
		Auth:            []ssh.AuthMethod{ssh.Password(password)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         12 * time.Second,
		Config: ssh.Config{
			KeyExchanges: []string{
				"diffie-hellman-group14-sha1",
				"diffie-hellman-group1-sha1",
				"diffie-hellman-group14-sha256",
				"curve25519-sha256",
				"curve25519-sha256@libssh.org",
			},
		},
		HostKeyAlgorithms: []string{ssh.KeyAlgoRSA, ssh.KeyAlgoDSA},
	}
	client, err := ssh.Dial("tcp", fmt.Sprintf("%s:%d", server.Host, port), cfg)
	if err != nil {
		return nil, err
	}
	p.clients[server.ID] = client
	_ = p.logs.Append(model.LogLevelInfo, "ssh", "connection ready: "+server.Name, server.ID)
	return client, nil
}

func (p *Pool) drop(serverID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if client := p.clients[serverID]; client != nil {
		_ = client.Close()
		delete(p.clients, serverID)
	}
}

func (p *Pool) resolve(serverRef string) (model.Server, model.Credential, string, error) {
	ref := strings.ToLower(strings.TrimSpace(serverRef))
	state := p.store.Snapshot()
	var server model.Server
	for _, item := range state.Servers {
		if strings.ToLower(item.ID) == ref ||
			strings.ToLower(item.Name) == ref ||
			strings.ToLower(item.Host) == ref ||
			strings.Contains(strings.ToLower(item.Name), ref) {
			server = item
			break
		}
	}
	if server.ID == "" {
		return model.Server{}, model.Credential{}, "", fmt.Errorf("server not found: %s", serverRef)
	}
	if server.Mode != "" && server.Mode != model.ConnectionModeDirectSSH {
		return model.Server{}, model.Credential{}, "", fmt.Errorf("server %s is not a direct SSH server", server.Name)
	}
	var credential model.Credential
	for _, item := range state.Credentials {
		if item.ID == server.CredentialRef {
			credential = item
			break
		}
	}
	if credential.ID == "" {
		return model.Server{}, model.Credential{}, "", fmt.Errorf("credential not found for server: %s", server.Name)
	}
	password, err := p.vault.Decrypt(credential.SecretCipher)
	if err != nil {
		return model.Server{}, model.Credential{}, "", err
	}
	return server, credential, password, nil
}

func alive(client *ssh.Client) bool {
	_, _, err := client.SendRequest("keepalive@opsera", true, nil)
	return err == nil
}
