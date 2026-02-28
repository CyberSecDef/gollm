package bus

import (
"sync"

"github.com/cybersecdef/gollm/internal/msgs"
)

const channelBuffer = 256

// Bus is a thread-safe publish/subscribe message bus.
// Agents subscribe with their ID; messages are routed by the To field.
// An empty To field means broadcast to all subscribers.
type Bus struct {
mu          sync.RWMutex
subscribers map[string]chan msgs.Message
}

// New creates a new Bus.
func New() *Bus {
return &Bus{
subscribers: make(map[string]chan msgs.Message),
}
}

// Subscribe registers an agent ID and returns the channel it will receive on.
// Calling Subscribe twice for the same ID is a no-op and returns the existing channel.
func (b *Bus) Subscribe(id string) <-chan msgs.Message {
b.mu.Lock()
defer b.mu.Unlock()
if ch, ok := b.subscribers[id]; ok {
return ch
}
ch := make(chan msgs.Message, channelBuffer)
b.subscribers[id] = ch
return ch
}

// Unsubscribe removes the subscriber and closes its channel.
func (b *Bus) Unsubscribe(id string) {
b.mu.Lock()
defer b.mu.Unlock()
if ch, ok := b.subscribers[id]; ok {
close(ch)
delete(b.subscribers, id)
}
}

// Publish sends a message to the addressed subscriber or broadcasts to all.
// Non-blocking: drops the message if the subscriber's buffer is full.
func (b *Bus) Publish(msg msgs.Message) {
b.mu.RLock()
defer b.mu.RUnlock()

if msg.To != "" {
if ch, ok := b.subscribers[msg.To]; ok {
select {
case ch <- msg:
default:
}
}
return
}

// Broadcast
for _, ch := range b.subscribers {
select {
case ch <- msg:
default:
}
}
}

// Send is an alias for Publish.
func (b *Bus) Send(msg msgs.Message) {
b.Publish(msg)
}
