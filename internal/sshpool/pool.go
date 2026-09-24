package sshpool

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
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
	shells  map[string]*Shell
}

type Shell struct {
	session      *ssh.Session
	stdin        io.WriteCloser
	outputWriter io.Closer
	reader       *bufio.Reader
	outputFile   *os.File
	mu           sync.Mutex
}

func New(store *config.Store, vault *crypto.Vault, logStore *logs.Store) *Pool {
	return &Pool{
		store:   store,
		vault:   vault,
		logs:    logStore,
		clients: map[string]*ssh.Client{},
		shells:  map[string]*Shell{},
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

func (p *Pool) RunShell(serverRef string, command string) (string, error) {
	server, credential, password, err := p.resolve(serverRef)
	if err != nil {
		return "", err
	}
	shell, err := p.shell(server, credential, password)
	if err != nil {
		return "", err
	}
	output, err := shell.Run(command)
	if err != nil {
		p.dropShell(server.ID)
		shell, retryErr := p.shell(server, credential, password)
		if retryErr != nil {
			return output, err
		}
		return shell.Run(command)
	}
	return output, nil
}

func (s *Shell) Run(command string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	marker := fmt.Sprintf("__OPSERA_DONE_%d__", time.Now().UnixNano())
	script := fmt.Sprintf("%s\nprintf '\\n%s:$?\\n'\n", command, marker)
	if _, err := io.WriteString(s.stdin, script); err != nil {
		return "", err
	}
	if s.outputFile != nil {
		_ = s.outputFile.SetReadDeadline(time.Now().Add(25 * time.Second))
	}
	var out strings.Builder
	for {
		line, err := s.reader.ReadString('\n')
		if len(line) > 0 {
			trimmed := strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(trimmed, marker+":") {
				codeText := strings.TrimPrefix(trimmed, marker+":")
				code, _ := strconv.Atoi(strings.TrimSpace(codeText))
				output := strings.TrimLeft(out.String(), "\r\n")
				if code != 0 {
					return output, fmt.Errorf("process exited with status %d", code)
				}
				return output, nil
			}
			out.WriteString(line)
		}
		if err != nil {
			if os.IsTimeout(err) {
				return out.String(), fmt.Errorf("command timed out waiting for shell marker")
			}
			return out.String(), err
		}
	}
}

func interactiveShellCommand(command string) string {
	return "bash -lic " + strconv.Quote(command)
}

func (p *Pool) CloseAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, shell := range p.shells {
		_ = shell.Close()
		delete(p.shells, id)
	}
	for id, client := range p.clients {
		_ = client.Close()
		delete(p.clients, id)
	}
}

func (s *Shell) Close() error {
	if s.stdin != nil {
		_, _ = io.WriteString(s.stdin, "exit\n")
		_ = s.stdin.Close()
	}
	if s.outputWriter != nil {
		_ = s.outputWriter.Close()
	}
	if s.session != nil {
		return s.session.Close()
	}
	return nil
}

func (p *Pool) shell(server model.Server, credential model.Credential, password string) (*Shell, error) {
	p.mu.Lock()
	if shell := p.shells[server.ID]; shell != nil {
		p.mu.Unlock()
		return shell, nil
	}
	p.mu.Unlock()
	client, err := p.client(server, credential, password)
	if err != nil {
		return nil, err
	}
	session, err := client.NewSession()
	if err != nil {
		p.drop(server.ID)
		client, err = p.client(server, credential, password)
		if err != nil {
			return nil, err
		}
		session, err = client.NewSession()
		if err != nil {
			return nil, err
		}
	}
	_ = session.RequestPty("xterm-256color", 40, 180, ssh.TerminalModes{
		ssh.ECHO:          0,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	})
	stdin, err := session.StdinPipe()
	if err != nil {
		_ = session.Close()
		return nil, err
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		_ = session.Close()
		return nil, err
	}
	session.Stdout = writer
	session.Stderr = writer
	if err := session.Start("bash --noprofile --norc"); err != nil {
		_ = writer.Close()
		_ = reader.Close()
		_ = session.Close()
		return nil, err
	}
	shell := &Shell{
		session:      session,
		stdin:        stdin,
		outputWriter: writer,
		reader:       bufio.NewReader(reader),
		outputFile:   reader,
	}
	initMarker := fmt.Sprintf("__OPSERA_INIT_%d__", time.Now().UnixNano())
	if _, err := io.WriteString(stdin, "export TERM=xterm-256color\nshopt -s expand_aliases\n[ -f ~/.bashrc ] && . ~/.bashrc >/dev/null 2>&1\nPS1=\nPROMPT_COMMAND=\nprintf '"+initMarker+"\\n'\n"); err != nil {
		_ = shell.Close()
		return nil, err
	}
	_ = reader.SetReadDeadline(time.Now().Add(15 * time.Second))
	for {
		line, err := shell.reader.ReadString('\n')
		if strings.TrimSpace(line) == initMarker {
			_ = reader.SetReadDeadline(time.Time{})
			break
		}
		if err != nil {
			_ = shell.Close()
			return nil, err
		}
	}
	p.mu.Lock()
	p.shells[server.ID] = shell
	p.mu.Unlock()
	_ = p.logs.Append(model.LogLevelInfo, "ssh", "interactive shell ready: "+server.Name, server.ID)
	return shell, nil
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
		HostKeyAlgorithms: []string{
			ssh.KeyAlgoED25519,
			ssh.KeyAlgoECDSA256,
			ssh.KeyAlgoECDSA384,
			ssh.KeyAlgoECDSA521,
			ssh.KeyAlgoRSASHA256,
			ssh.KeyAlgoRSASHA512,
			ssh.KeyAlgoRSA,
			ssh.InsecureKeyAlgoDSA,
		},
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
	p.dropShell(serverID)
	p.mu.Lock()
	defer p.mu.Unlock()
	if client := p.clients[serverID]; client != nil {
		_ = client.Close()
		delete(p.clients, serverID)
	}
}

func (p *Pool) dropShell(serverID string) {
	p.mu.Lock()
	shell := p.shells[serverID]
	delete(p.shells, serverID)
	p.mu.Unlock()
	if shell != nil {
		_ = shell.Close()
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
