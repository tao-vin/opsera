package model

type CommandStatus string

const (
	CommandStatusQueued CommandStatus = "queued"
	CommandStatusDone   CommandStatus = "done"
	CommandStatusFailed CommandStatus = "failed"
)

type Command struct {
	ID        string        `json:"id"`
	SessionID string        `json:"sessionId,omitempty"`
	Target    string        `json:"target,omitempty"`
	Command   string        `json:"command"`
	Status    CommandStatus `json:"status"`
	Output    string        `json:"output,omitempty"`
	Error     string        `json:"error,omitempty"`
	CreatedAt string        `json:"createdAt"`
	UpdatedAt string        `json:"updatedAt,omitempty"`
}
