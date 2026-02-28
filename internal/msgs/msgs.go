// Package msgs defines the shared message types used by all agents and the bus.
package msgs

import "time"

// MessageType identifies the kind of message exchanged between agents.
type MessageType string

const (
	MsgTypeUserInput     MessageType = "user_input"
	MsgTypeTaskAssign    MessageType = "task_assign"
	MsgTypeTaskResult    MessageType = "task_result"
	MsgTypeStatusUpdate  MessageType = "status_update"
	MsgTypeToolCall      MessageType = "tool_call"
	MsgTypeToolResult    MessageType = "tool_result"
	MsgTypeQuestion      MessageType = "question"
	MsgTypeAnswer        MessageType = "answer"
	MsgTypeEvent         MessageType = "event"
	MsgTypeShutdown      MessageType = "shutdown"
	MsgTypeFinalResponse MessageType = "final_response"
	MsgTypeError         MessageType = "error"
	MsgTypeStopRequest   MessageType = "stop_request"
)

// Message is the unit of communication on the message bus.
type Message struct {
	ID            string
	Type          MessageType
	From          string
	To            string // empty = broadcast
	Content       string
	Metadata      map[string]string
	Timestamp     time.Time
	CorrelationID string // links related messages across an orchestration run
}

// AgentStatus represents the lifecycle state of an agent.
type AgentStatus string

const (
	StatusIdle      AgentStatus = "idle"
	StatusWorking   AgentStatus = "working"
	StatusWaiting   AgentStatus = "waiting"
	StatusCompleted AgentStatus = "completed"
	StatusError     AgentStatus = "error"
)

// NewMessage constructs a Message with a generated ID and current timestamp.
func NewMessage(msgType MessageType, from, to, content string) Message {
	return Message{
		ID:        newID(),
		Type:      msgType,
		From:      from,
		To:        to,
		Content:   content,
		Metadata:  make(map[string]string),
		Timestamp: time.Now(),
	}
}

// newID generates a short unique identifier from the current time.
func newID() string {
	return time.Now().Format("20060102150405.000000000")
}
