package agents

import (
	"context"

	"github.com/cybersecdef/gollm/internal/msgs"
)

// Re-export shared types so callers can use agents.Message etc.
type (
	Message     = msgs.Message
	MessageType = msgs.MessageType
	AgentStatus = msgs.AgentStatus
)

const (
	MsgTypeUserInput     = msgs.MsgTypeUserInput
	MsgTypeTaskAssign    = msgs.MsgTypeTaskAssign
	MsgTypeTaskResult    = msgs.MsgTypeTaskResult
	MsgTypeStatusUpdate  = msgs.MsgTypeStatusUpdate
	MsgTypeToolCall      = msgs.MsgTypeToolCall
	MsgTypeToolResult    = msgs.MsgTypeToolResult
	MsgTypeQuestion      = msgs.MsgTypeQuestion
	MsgTypeAnswer        = msgs.MsgTypeAnswer
	MsgTypeEvent         = msgs.MsgTypeEvent
	MsgTypeShutdown      = msgs.MsgTypeShutdown
	MsgTypeFinalResponse = msgs.MsgTypeFinalResponse
	MsgTypeError         = msgs.MsgTypeError

	StatusIdle      = msgs.StatusIdle
	StatusWorking   = msgs.StatusWorking
	StatusWaiting   = msgs.StatusWaiting
	StatusCompleted = msgs.StatusCompleted
	StatusError     = msgs.StatusError
)

// NewMessage delegates to msgs.NewMessage.
var NewMessage = msgs.NewMessage

// Agent is the common interface for all agent types.
type Agent interface {
	ID() string
	Role() string
	Status() AgentStatus
	// Inbox returns the receive-only channel on which this agent receives messages.
	Inbox() <-chan Message
	// Scratchpad returns the agent's current private working notes.
	Scratchpad() string
	Start(ctx context.Context) error
	Stop()
	// Send publishes a message onto the shared bus (outbox).
	Send(msg Message)
}
