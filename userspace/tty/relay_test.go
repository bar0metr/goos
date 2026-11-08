package tty

import (
	"strings"
	"testing"

	"goos/kernel/kcall"
)

// ringSource hands out queued output the way a stream does: at most one
// buffer per read, then empty.
type ringSource struct {
	queued []byte
	reads  int
	closed bool
}

func (source *ringSource) ReadOutput(buffer []byte) (int, bool, bool) {
	source.reads++
	if len(source.queued) == 0 {
		return 0, true, source.closed
	}
	count := copy(buffer, source.queued)
	source.queued = source.queued[count:]
	return count, false, false
}

// fullRing is the output a client leaves unread when it fills the ring and
// then sends a request: lines of 64 bytes, numbered.
func fullRing() string {
	var text strings.Builder
	for line := 0; text.Len()+64 <= kcall.StreamCapacity; line++ {
		row := "line " + string(rune('A'+line%26)) + strings.Repeat(".", 56) + "\n"
		text.WriteString(row[:63] + "\n")
	}
	return text.String()
}

func TestRelayBeforeARequestPutsAFullRingAheadOfThePrompt(t *testing.T) {
	h := newEngineHarness()
	output := fullRing()
	source := &ringSource{queued: []byte(output)}
	buffer := make([]byte, kcall.MaxStreamIO)
	relayed, closed := h.engine.Relay(source, buffer, kcall.StreamCapacity)
	if !relayed || closed || len(source.queued) != 0 {
		t.Fatalf("relayed=%v closed=%v left=%d", relayed, closed, len(source.queued))
	}
	h.request(t, OperationReadLine, "goos> ", 1)
	if got := h.output.String(); got != output+"goos> " {
		t.Fatalf("terminal got %d bytes, want the output then the prompt; tail %q", len(got), got[max(0, len(got)-80):])
	}
}

func TestRelayWithABatchLimitLeavesTheRest(t *testing.T) {
	// The per-pass bound: output past it reaches the terminal under a prompt
	// shown meanwhile, which hides and redraws the prompt around it.
	h := newEngineHarness()
	output := fullRing()
	source := &ringSource{queued: []byte(output)}
	buffer := make([]byte, kcall.MaxStreamIO)
	h.engine.Relay(source, buffer, 4096)
	if left := len(source.queued); left != len(output)-4096 {
		t.Fatalf("left %d bytes, want %d", left, len(output)-4096)
	}
	h.request(t, OperationReadLine, "goos> ", 1)
	h.engine.Relay(source, buffer, kcall.StreamCapacity)
	if got := h.output.String(); !strings.Contains(got, "goos> \r\x1b[K") {
		t.Fatalf("late output did not redraw the prompt: %q", got[max(0, len(got)-80):])
	}
}

func TestRelayStopsAtTheLimitAndAtTheEnd(t *testing.T) {
	h := newEngineHarness()
	source := &ringSource{queued: []byte(strings.Repeat("x", 1000))}
	buffer := make([]byte, 100)
	if relayed, closed := h.engine.Relay(source, buffer, 250); !relayed || closed || source.reads != 3 {
		t.Fatalf("relayed=%v closed=%v reads=%d", relayed, closed, source.reads)
	}
	source = &ringSource{closed: true}
	if relayed, closed := h.engine.Relay(source, buffer, 250); relayed || !closed {
		t.Fatalf("ended stream: relayed=%v closed=%v", relayed, closed)
	}
	source = &ringSource{}
	if relayed, closed := h.engine.Relay(source, buffer, 250); relayed || closed || source.reads != 1 {
		t.Fatalf("empty stream: relayed=%v closed=%v reads=%d", relayed, closed, source.reads)
	}
}

type brokenSource struct{ count int }

func (source brokenSource) ReadOutput(buffer []byte) (int, bool, bool) {
	return source.count, false, false
}

func TestRelayTreatsAnImpossibleReadAsAnEndedStream(t *testing.T) {
	h := newEngineHarness()
	buffer := make([]byte, 16)
	for _, count := range []int{0, 17} {
		if relayed, closed := h.engine.Relay(brokenSource{count: count}, buffer, 64); relayed || !closed {
			t.Fatalf("read of %d bytes: relayed=%v closed=%v", count, relayed, closed)
		}
	}
}
