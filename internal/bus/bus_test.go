package bus

import (
	"sync"
	"testing"
	"time"

	"github.com/cybersecdef/gollm/internal/msgs"
)

func newMsg(from, to, content string) msgs.Message {
	return msgs.NewMessage(msgs.MsgTypeEvent, from, to, content)
}

// ---- Subscribe ---------------------------------------------------------------

func TestSubscribe_ReturnsSameChannelOnDuplicate(t *testing.T) {
	b := New()
	ch1 := b.Subscribe("agent-a")
	ch2 := b.Subscribe("agent-a")
	if ch1 != ch2 {
		t.Error("expected Subscribe with same ID to return the same channel")
	}
}

func TestSubscribe_ReturnsDifferentChannelsForDifferentIDs(t *testing.T) {
	b := New()
	ch1 := b.Subscribe("agent-a")
	ch2 := b.Subscribe("agent-b")
	if ch1 == ch2 {
		t.Error("expected different channels for different subscriber IDs")
	}
}

// ---- Publish / directed -------------------------------------------------------

func TestPublish_DirectedMessage_DeliveredToRecipient(t *testing.T) {
	b := New()
	ch := b.Subscribe("recipient")

	b.Publish(newMsg("sender", "recipient", "hello"))

	select {
	case got := <-ch:
		if got.Content != "hello" {
			t.Errorf("unexpected content: %q", got.Content)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for directed message")
	}
}

func TestPublish_DirectedMessage_NotDeliveredToOther(t *testing.T) {
	b := New()
	_ = b.Subscribe("recipient")
	other := b.Subscribe("bystander")

	b.Publish(newMsg("sender", "recipient", "secret"))

	select {
	case m := <-other:
		t.Errorf("bystander should not receive directed message, got: %+v", m)
	case <-time.After(100 * time.Millisecond):
		// Good: no message delivered to bystander.
	}
}

func TestPublish_DirectedMessage_UnknownRecipient_Dropped(t *testing.T) {
	b := New()
	// No subscribers – publish should not block or panic.
	b.Publish(newMsg("src", "nobody", "msg"))
}

// ---- Broadcast ----------------------------------------------------------------

func TestPublish_Broadcast_DeliveredToAllSubscribers(t *testing.T) {
	b := New()
	ch1 := b.Subscribe("a")
	ch2 := b.Subscribe("b")
	ch3 := b.Subscribe("c")

	b.Publish(newMsg("src", "", "broadcast"))

	for _, ch := range []<-chan msgs.Message{ch1, ch2, ch3} {
		select {
		case got := <-ch:
			if got.Content != "broadcast" {
				t.Errorf("unexpected content: %q", got.Content)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for broadcast message")
		}
	}
}

// ---- Unsubscribe --------------------------------------------------------------

func TestUnsubscribe_ChannelIsClosed(t *testing.T) {
	b := New()
	ch := b.Subscribe("agent")
	b.Unsubscribe("agent")

	select {
	case _, ok := <-ch:
		if ok {
			t.Error("expected channel to be closed after Unsubscribe")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for channel close")
	}
}

func TestUnsubscribe_UnknownID_NoOp(t *testing.T) {
	b := New()
	// Should not panic.
	b.Unsubscribe("nobody")
}

func TestUnsubscribe_DirectedMessageAfterUnsubscribe_Dropped(t *testing.T) {
	b := New()
	b.Subscribe("agent")
	b.Unsubscribe("agent")

	// Should not panic or block.
	b.Publish(newMsg("src", "agent", "after-unsub"))
}

// ---- Send alias ---------------------------------------------------------------

func TestSend_IsAliasForPublish(t *testing.T) {
	b := New()
	ch := b.Subscribe("x")

	b.Send(newMsg("s", "x", "via-send"))

	select {
	case got := <-ch:
		if got.Content != "via-send" {
			t.Errorf("unexpected content: %q", got.Content)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for message from Send")
	}
}

// ---- Backpressure / bounded channel ------------------------------------------

func TestPublish_Backpressure_DropsWhenFull(t *testing.T) {
	b := New()
	ch := b.Subscribe("slow-consumer")

	// Fill the channel buffer (channelBuffer = 256).
	for i := 0; i < channelBuffer+10; i++ {
		b.Publish(newMsg("src", "slow-consumer", "flood"))
	}

	// The channel must not be more than channelBuffer deep.
	if len(ch) > channelBuffer {
		t.Errorf("channel depth %d exceeds buffer size %d", len(ch), channelBuffer)
	}
}

// ---- Concurrency safety -------------------------------------------------------

func TestPublish_ConcurrentPublishers_NoPanic(t *testing.T) {
	b := New()
	ch := b.Subscribe("consumer")
	defer b.Unsubscribe("consumer")

	const publishers = 20
	var wg sync.WaitGroup
	for i := 0; i < publishers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.Publish(newMsg("p", "consumer", "concurrent"))
		}()
	}
	wg.Wait()

	// Drain whatever was buffered.
	deadline := time.After(500 * time.Millisecond)
	for {
		select {
		case <-ch:
		case <-deadline:
			return
		}
	}
}

func TestSubscribeUnsubscribe_Concurrent_NoPanic(t *testing.T) {
	b := New()
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		id := "agent"
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch := b.Subscribe(id)
			b.Publish(newMsg("src", "", "msg"))
			select {
			case <-ch:
			default:
			}
			b.Unsubscribe(id)
		}()
	}
	wg.Wait()
}
