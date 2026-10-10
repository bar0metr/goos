package tty

import (
	"strings"
	"testing"

	"goos/kernel/ipc"
)

type engineHarness struct {
	engine      *Engine
	output      strings.Builder
	replies     []ipc.Message
	maxFragment int
}

func newEngineHarness() *engineHarness {
	h := &engineHarness{}
	h.engine = NewEngine(func(data []byte) bool {
		if len(data) > h.maxFragment {
			h.maxFragment = len(data)
		}
		h.output.Write(data)
		return true
	}, func(message ipc.Message) bool { h.replies = append(h.replies, message); return true })
	return h
}

func (h *engineHarness) request(t *testing.T, operation Operation, prompt string, correlation uint64) {
	t.Helper()
	request, ok := Request(operation, []byte(prompt), correlation)
	if !ok {
		t.Fatal("invalid test request")
	}
	h.engine.Request(&request)
}

func (h *engineHarness) feed(text string) {
	for index := 0; index < len(text); index++ {
		h.engine.Feed(text[index])
	}
	h.engine.FlushInput()
}

func (h *engineHarness) reply(t *testing.T, index int, expected Result, text string, correlation uint64) {
	t.Helper()
	if index >= len(h.replies) {
		t.Fatalf("reply %d missing: %d replies", index, len(h.replies))
	}
	message := h.replies[index]
	result, data, ok := DecodeReply(&message)
	if !ok || result != expected || string(data) != text || message.Correlation != correlation {
		t.Fatalf("reply %d = %d %q correlation=%d valid=%v", index, result, data, message.Correlation, ok)
	}
}

func TestEngineLineEditingBackgroundOutputAndRedraw(t *testing.T) {
	h := newEngineHarness()
	h.request(t, OperationReadLine, "goos> ", 1)
	h.feed("bad\x15echo fore")
	h.output.Reset()
	h.engine.Output([]byte("[1] done\n"))
	if got := h.output.String(); got != "\r\x1b[K[1] done\ngoos> echo fore" {
		t.Fatalf("background output %q", got)
	}
	h.request(t, OperationRedraw, "$ ", 2)
	h.reply(t, 0, ResultAccepted, "", 2)
	h.feed("ground\n")
	h.reply(t, 1, ResultLine, "echo foreground", 1)
	if h.engine.Failed() {
		t.Fatal("engine failed")
	}
}

func TestEngineQueuedInterruptEndAndTypedAhead(t *testing.T) {
	h := newEngineHarness()
	h.feed("discard")
	h.engine.Interrupt()
	h.request(t, OperationReadLine, "> ", 1)
	h.reply(t, 0, ResultInterrupt, "", 1)
	h.feed("\x04")
	h.request(t, OperationReadLine, "> ", 2)
	h.reply(t, 1, ResultEndOfInput, "", 2)
	h.feed("echo ahead\n")
	h.output.Reset()
	h.request(t, OperationReadLine, "> ", 3)
	h.reply(t, 2, ResultLine, "echo ahead", 3)
	if got := h.output.String(); got != "> echo ahead\n" {
		t.Fatalf("typed-ahead prompt %q", got)
	}
}

func TestEngineMultipartLineAndInterruptDuringContinue(t *testing.T) {
	h := newEngineHarness()
	text := strings.Repeat("x", MaxLine)
	h.request(t, OperationReadLine, "", 1)
	h.feed(text + "\n")
	h.reply(t, 0, ResultPart, text[:MaxPart], 1)
	h.request(t, OperationContinue, "", 2)
	h.reply(t, 1, ResultPart, text[MaxPart:2*MaxPart], 2)
	h.engine.Interrupt()
	h.request(t, OperationContinue, "", 3)
	h.reply(t, 2, ResultInterrupt, "", 3)
	h.request(t, OperationContinue, "", 4)
	h.reply(t, 3, ResultRefused, "", 4)

	h.request(t, OperationReadLine, "", 5)
	h.feed(text + "\n")
	position, replyIndex, correlation := MaxPart, 5, uint64(6)
	for position < len(text) {
		h.request(t, OperationContinue, "", correlation)
		end := position + MaxPart
		result := ResultPart
		if end >= len(text) {
			end, result = len(text), ResultLine
		}
		h.reply(t, replyIndex, result, text[position:end], correlation)
		position, replyIndex, correlation = end, replyIndex+1, correlation+1
	}
}

func TestEngineRefusesConcurrentReadsAndMalformedRequests(t *testing.T) {
	h := newEngineHarness()
	h.request(t, OperationReadLine, "> ", 1)
	h.request(t, OperationReadLine, "wrong ", 2)
	h.reply(t, 0, ResultRefused, "", 2)
	message, _ := Request(OperationGeometry, nil, 3)
	message.Version--
	h.engine.Request(&message)
	h.reply(t, 1, ResultRefused, "", 3)
	h.feed("ok\n")
	h.reply(t, 2, ResultLine, "ok", 1)
}

func TestEngineRawBytesInterruptsAndReset(t *testing.T) {
	for _, inBand := range []bool{false, true} {
		h := newEngineHarness()
		h.request(t, OperationRawMode, "", 1)
		h.request(t, OperationReadLine, "", 2)
		h.feed("\x1b[A\x04\x00")
		h.reply(t, 0, ResultAccepted, "", 1)
		h.reply(t, 1, ResultRaw, "\x1b[A\x04\x00", 2)
		h.feed("stale")
		if inBand {
			h.engine.Feed(0x03)
		} else {
			h.engine.Interrupt()
		}
		h.request(t, OperationReadLine, "", 3)
		h.reply(t, 2, ResultInterrupt, "", 3)
		h.request(t, OperationReadLine, "", 4)
		if len(h.replies) != 3 {
			t.Fatal("pre-interrupt raw bytes survived")
		}
		h.engine.Interrupt()
		h.reply(t, 3, ResultInterrupt, "", 4)
		h.output.Reset()
		h.request(t, OperationLineMode, "", 5)
		if h.engine.Raw() || h.output.String() != ResetSequence {
			t.Fatalf("reset = %q raw=%v", h.output.String(), h.engine.Raw())
		}
	}
}

func TestEngineSessionReplacementKeepsEditedLineAndEndsRawMode(t *testing.T) {
	h := newEngineHarness()
	h.request(t, OperationReadLine, "old> ", 1)
	h.feed("kept")
	h.request(t, OperationRawMode, "", 2)
	h.feed("discarded")
	h.engine.CloseSession()
	if h.engine.Raw() || !strings.Contains(h.output.String(), ResetSequence) {
		t.Fatal("close did not reset raw terminal")
	}
	h.engine.Interrupt()
	h.engine.ReplaceSession()
	h.request(t, OperationReadLine, "new> ", 3)
	h.feed("ok\n")
	h.reply(t, 2, ResultLine, "ok", 3)

	h = newEngineHarness()
	h.feed("kept")
	h.engine.CloseSession()
	h.engine.ReplaceSession()
	h.request(t, OperationReadLine, "new> ", 4)
	h.feed("\n")
	h.reply(t, 0, ResultLine, "kept", 4)
}

func TestEngineInstancesAreIndependent(t *testing.T) {
	a, b := newEngineHarness(), newEngineHarness()
	a.request(t, OperationReadLine, "a> ", 1)
	b.request(t, OperationReadLine, "b> ", 1)
	a.feed("one")
	b.feed("two\n")
	b.reply(t, 0, ResultLine, "two", 1)
	a.feed("\n")
	a.reply(t, 0, ResultLine, "one", 1)
}

func TestEngineBackpressureStopsFurtherWritesAndReplies(t *testing.T) {
	writes, replies := 0, 0
	engine := NewEngine(func(data []byte) bool { writes++; return false }, func(ipc.Message) bool { replies++; return true })
	engine.Output([]byte("blocked"))
	engine.Feed('x')
	request, _ := Request(OperationReadLine, []byte("> "), 1)
	engine.Request(&request)
	engine.Output([]byte("ignored"))
	if !engine.Failed() || writes != 1 || replies != 0 {
		t.Fatalf("failed=%v writes=%d replies=%d", engine.Failed(), writes, replies)
	}

	engine = NewEngine(func([]byte) bool { return true }, func(ipc.Message) bool { return false })
	request, _ = Request(OperationRawMode, nil, 1)
	engine.Request(&request)
	if !engine.Failed() {
		t.Fatal("reply failure not propagated")
	}
}

func TestEngineOutputFragmentsAreBounded(t *testing.T) {
	h := newEngineHarness()
	h.engine.Output([]byte(strings.Repeat("x", 3*MaxPart)))
	if h.maxFragment > MaxPart || h.engine.Failed() {
		t.Fatalf("fragment=%d failed=%v", h.maxFragment, h.engine.Failed())
	}
}
