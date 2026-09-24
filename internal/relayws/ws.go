package relayws

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

type Message struct {
	Type       string `json:"type"`
	ID         string `json:"id,omitempty"`
	Command    string `json:"command,omitempty"`
	Path       string `json:"path,omitempty"`
	Data       string `json:"data,omitempty"`
	FileID     string `json:"fileId,omitempty"`
	SHA256     string `json:"sha256,omitempty"`
	Size       int64  `json:"size,omitempty"`
	PartSize   int64  `json:"partSize,omitempty"`
	PartCount  int    `json:"partCount,omitempty"`
	RelayURL   string `json:"relayUrl,omitempty"`
	ExitCode   int    `json:"exitCode,omitempty"`
	Output     string `json:"output,omitempty"`
	Error      string `json:"error,omitempty"`
	Restart    bool   `json:"restart,omitempty"`
	Executable string `json:"executable,omitempty"`
}

type Conn struct {
	c      net.Conn
	r      *bufio.Reader
	masked bool
}

func Dial(rawURL string) (*Conn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		if u.Scheme == "wss" {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	var c net.Conn
	if u.Scheme == "wss" {
		c, err = tls.Dial("tcp", host, &tls.Config{ServerName: u.Hostname()})
	} else {
		c, err = net.Dial("tcp", host)
	}
	if err != nil {
		return nil, err
	}
	keyRaw := make([]byte, 16)
	if _, err := rand.Read(keyRaw); err != nil {
		_ = c.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyRaw)
	path := u.RequestURI()
	if path == "" {
		path = "/"
	}
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", path, u.Host, key)
	if _, err := io.WriteString(c, req); err != nil {
		_ = c.Close()
		return nil, err
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = c.Close()
		return nil, fmt.Errorf("websocket upgrade failed: %s", resp.Status)
	}
	if resp.Header.Get("Sec-WebSocket-Accept") != acceptKey(key) {
		_ = c.Close()
		return nil, fmt.Errorf("invalid websocket accept")
	}
	_ = c.SetDeadline(time.Time{})
	return &Conn{c: c, r: br, masked: true}, nil
}

func Upgrade(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	if !headerContains(r.Header, "Connection", "upgrade") || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return nil, fmt.Errorf("not a websocket upgrade")
	}
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if key == "" {
		return nil, fmt.Errorf("missing websocket key")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, fmt.Errorf("hijacker unavailable")
	}
	netConn, brw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + acceptKey(key) + "\r\n\r\n"
	if _, err := brw.WriteString(resp); err != nil {
		_ = netConn.Close()
		return nil, err
	}
	if err := brw.Flush(); err != nil {
		_ = netConn.Close()
		return nil, err
	}
	_ = netConn.SetDeadline(time.Time{})
	return &Conn{c: netConn, r: brw.Reader, masked: false}, nil
}

func (c *Conn) Close() error {
	return c.c.Close()
}

func (c *Conn) SetDeadline(t time.Time) error {
	return c.c.SetDeadline(t)
}

func (c *Conn) ReadJSON(v any) error {
	_, payload, err := c.ReadMessage()
	if err != nil {
		return err
	}
	return json.Unmarshal(payload, v)
}

func (c *Conn) WriteJSON(v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.WriteMessage(1, payload)
}

func (c *Conn) ReadMessage() (byte, []byte, error) {
	for {
		header := make([]byte, 2)
		if _, err := io.ReadFull(c.r, header); err != nil {
			return 0, nil, err
		}
		opcode := header[0] & 0x0f
		masked := header[1]&0x80 != 0
		length := uint64(header[1] & 0x7f)
		switch length {
		case 126:
			var b [2]byte
			if _, err := io.ReadFull(c.r, b[:]); err != nil {
				return 0, nil, err
			}
			length = uint64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			if _, err := io.ReadFull(c.r, b[:]); err != nil {
				return 0, nil, err
			}
			length = binary.BigEndian.Uint64(b[:])
		}
		var mask [4]byte
		if masked {
			if _, err := io.ReadFull(c.r, mask[:]); err != nil {
				return 0, nil, err
			}
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(c.r, payload); err != nil {
			return 0, nil, err
		}
		if masked {
			for i := range payload {
				payload[i] ^= mask[i%4]
			}
		}
		if opcode == 8 {
			return 0, nil, io.EOF
		}
		if opcode == 9 {
			_ = c.writeControl(10, payload)
			continue
		}
		if opcode == 1 || opcode == 2 {
			return opcode, payload, nil
		}
	}
}

func (c *Conn) WriteMessage(opcode byte, payload []byte) error {
	return c.write(opcode, payload)
}

func (c *Conn) WriteBinary(payload []byte) error {
	return c.write(2, payload)
}

func (c *Conn) WriteText(text string) error {
	return c.write(1, []byte(text))
}

func (c *Conn) writeControl(opcode byte, payload []byte) error {
	return c.write(opcode, payload)
}

func (c *Conn) write(opcode byte, payload []byte) error {
	var out bytes.Buffer
	out.WriteByte(0x80 | opcode)
	maskBit := byte(0)
	if c.masked {
		maskBit = 0x80
	}
	n := len(payload)
	switch {
	case n < 126:
		out.WriteByte(maskBit | byte(n))
	case n <= 65535:
		out.WriteByte(maskBit | 126)
		var b [2]byte
		binary.BigEndian.PutUint16(b[:], uint16(n))
		out.Write(b[:])
	default:
		out.WriteByte(maskBit | 127)
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(n))
		out.Write(b[:])
	}
	if c.masked {
		var mask [4]byte
		if _, err := rand.Read(mask[:]); err != nil {
			return err
		}
		out.Write(mask[:])
		encoded := make([]byte, len(payload))
		for i := range payload {
			encoded[i] = payload[i] ^ mask[i%4]
		}
		payload = encoded
	}
	out.Write(payload)
	_, err := c.c.Write(out.Bytes())
	return err
}

func acceptKey(key string) string {
	sum := sha1.Sum([]byte(key + GUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func headerContains(h http.Header, key string, value string) bool {
	for _, item := range h.Values(key) {
		for _, part := range strings.Split(item, ",") {
			if strings.EqualFold(strings.TrimSpace(part), value) {
				return true
			}
		}
	}
	return false
}
