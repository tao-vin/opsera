package model

type LogLevel string

const (
	LogLevelInfo  LogLevel = "info"
	LogLevelWarn  LogLevel = "warn"
	LogLevelError LogLevel = "error"
)

type LogEntry struct {
	ID        string   `json:"id"`
	Timestamp string   `json:"timestamp"`
	Level     LogLevel `json:"level"`
	Source    string   `json:"source"`
	Message   string   `json:"message"`
	ServerID  string   `json:"serverId,omitempty"`
}
