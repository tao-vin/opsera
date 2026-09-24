package model

type SessionStatus string

const (
	SessionStatusReady    SessionStatus = "ready"
	SessionStatusCaptured SessionStatus = "captured"
	SessionStatusError    SessionStatus = "error"
)

type Session struct {
	ID        string        `json:"id"`
	ServerID  string        `json:"serverId"`
	Server    string        `json:"server"`
	Target    string        `json:"target"`
	LaunchArg string        `json:"launchArg,omitempty"`
	Source    string        `json:"source"`
	Status    SessionStatus `json:"status"`
	Message   string        `json:"message,omitempty"`
	StartedAt string        `json:"startedAt"`
}
