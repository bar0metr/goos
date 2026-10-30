package tty

import (
	"strings"
	"testing"

	"goos/kernel/ipc"
)

// probeEngine is an engine in transport mode whose shell subscribed to
// geometry, recording what it wrote and sent.
func probeEngine(t *testing.T) (*Engine, *strings.Builder, *[]ipc.Message) {
	t.Helper()
	written := &strings.Builder{}
	sent := &[]ipc.Message{}
	engine := NewEngine(func(data []byte) bool {
		written.Write(data)
		return true
	}, func(message ipc.Message) bool {
		*sent = append(*sent, message)
		return true
	})
	for correlation, operation := range []Operation{OperationGeometry, OperationTransportMode} {
		message, ok := Request(operation, nil, uint64(correlation+1))
		if !ok {
			t.Fatal("request refused")
		}
		engine.Request(&message)
	}
	engine.Output([]byte("\x1b[0m"))
	written.Reset()
	*sent = nil
	return engine, written, sent
}

func TestProbeLeavesPlainOutputAlone(t *testing.T) {
	written := &strings.Builder{}
	engine := NewEngine(func(data []byte) bool {
		written.Write(data)
		return true
	}, func(ipc.Message) bool { return true })
	message, _ := Request(OperationTransportMode, nil, 1)
	engine.Request(&message)
	engine.Output([]byte("hello\n"))
	if engine.ProbeActive() {
		t.Fatal("probing a program that draws no screen")
	}
	engine.Output([]byte("\x1b[H"))
	if !engine.ProbeActive() {
		t.Fatal("not probing a program that draws")
	}
}

func TestProbeCountsWhatAJobWroteBeforeTransportMode(t *testing.T) {
	engine := NewEngine(func([]byte) bool { return true }, func(ipc.Message) bool { return true })
	engine.Output([]byte("\x1b[H"))
	message, _ := Request(OperationTransportMode, nil, 1)
	engine.Request(&message)
	if !engine.ProbeActive() {
		t.Fatal("a job that drew before the mode changed is not probed")
	}
}

func feedAll(engine *Engine, text string) {
	for index := 0; index < len(text); index++ {
		engine.Feed(text[index])
	}
	engine.FlushInput()
}

func TestProbeAnswerResizesWithoutReachingTheProgram(t *testing.T) {
	engine, written, sent := probeEngine(t)
	engine.ProbeTick()
	if written.String() != probeSequence {
		t.Fatalf("probe wrote %q", written.String())
	}
	written.Reset()
	feedAll(engine, "\x1b[40;120R")
	geometry := engine.CurrentGeometry()
	if geometry.Rows != 40 || geometry.Columns != 120 || geometry.Authoritative || geometry.Display {
		t.Fatalf("geometry = %+v", geometry)
	}
	if engine.rawInput.Len() != 0 || written.Len() != 0 {
		t.Fatalf("answer leaked: %d input bytes, wrote %q", engine.rawInput.Len(), written.String())
	}
	engine.FlushGeometry()
	if len(*sent) != 1 {
		t.Fatalf("%d notices", len(*sent))
	}
	if notice, valid := DecodeGeometryNotice(&(*sent)[0]); !valid || notice.Rows != 40 || notice.Columns != 120 {
		t.Fatalf("notice = %+v, %v", notice, valid)
	}
}

func TestProbeAnswerWithTheSameSizeNotifiesNobody(t *testing.T) {
	engine, _, sent := probeEngine(t)
	engine.ProbeTick()
	sequence := engine.CurrentGeometry().Sequence
	feedAll(engine, "\x1b[25;80R")
	engine.FlushGeometry()
	if engine.CurrentGeometry().Sequence != sequence || len(*sent) != 0 {
		t.Fatalf("sequence %d, %d notices", engine.CurrentGeometry().Sequence, len(*sent))
	}
}

func TestProbeKeepsKeysTypedAroundTheAnswer(t *testing.T) {
	engine, _, _ := probeEngine(t)
	engine.ProbeTick()
	// A key, an arrow, and Shift-F3 on row 1 are all input; the answer behind
	// them is not.
	feedAll(engine, "a\x1b[A\x1b[1;2R\x1b\x1b[30;90R")
	var target [MaxPart]byte
	count := engine.rawInput.Take(target[:])
	if string(target[:count]) != "a\x1b[A\x1b[1;2R\x1b" {
		t.Fatalf("input = %q", target[:count])
	}
	if geometry := engine.CurrentGeometry(); geometry.Rows != 30 || geometry.Columns != 90 {
		t.Fatalf("geometry = %+v", geometry)
	}
}

func TestProbeReleasesALoneEscapeAndStopsAfterSilence(t *testing.T) {
	engine, written, _ := probeEngine(t)
	engine.ProbeTick()
	engine.Feed(0x1b)
	if engine.rawInput.Len() != 0 {
		t.Fatal("ESC was not held for the answer")
	}
	engine.ProbeTick()
	if engine.rawInput.Len() != 1 {
		t.Fatal("ESC stayed held after the wait")
	}
	for tick := 0; tick < 20; tick++ {
		engine.ProbeTick()
	}
	if count := strings.Count(written.String(), probeSequence); count != probeUnanswered {
		t.Fatalf("%d probes to a silent terminal", count)
	}
	if engine.ProbeActive() {
		t.Fatal("still probing a silent terminal")
	}
}

func TestProbeWaitsForTheEndOfASequence(t *testing.T) {
	engine, written, _ := probeEngine(t)
	for _, partial := range []string{"\x1b[3", "\x1b]0;title", "\xe2\x94"} {
		engine.Output([]byte(partial))
		written.Reset()
		engine.ProbeTick()
		if written.Len() != 0 {
			t.Fatalf("probed inside %q", partial)
		}
		engine.probe = sizeProbe{}
	}
	engine.Output([]byte("\x1b[31m\xe2\x94\x80text"))
	written.Reset()
	engine.ProbeTick()
	if written.String() != probeSequence {
		t.Fatalf("no probe after whole output: %q", written.String())
	}
}

func TestProbeOnlyForATerminalThatCannotSayItsSize(t *testing.T) {
	for _, geometry := range []Geometry{{Columns: 80, Rows: 25, Display: true, Authoritative: true}, {Columns: 80, Rows: 25, Authoritative: true}} {
		engine, _, _ := probeEngine(t)
		engine.SetGeometry(geometry)
		if engine.ProbeActive() {
			t.Fatalf("probing %+v", geometry)
		}
	}
	engine := NewEngine(func([]byte) bool { return true }, func(ipc.Message) bool { return true })
	message, _ := Request(OperationRawMode, nil, 1)
	engine.Request(&message)
	if engine.ProbeActive() {
		t.Fatal("probing beside a native program, which asks for itself")
	}
}

func TestLateAnswerAfterTheJobIsNotALine(t *testing.T) {
	engine, written, _ := probeEngine(t)
	engine.ProbeTick()
	message, _ := Request(OperationLineMode, nil, 9)
	engine.Request(&message)
	written.Reset()
	feedAll(engine, "\x1b[33;99R")
	if len(engine.line.Pending()) != 0 || strings.Contains(written.String(), "33") {
		t.Fatalf("answer entered the line: %q", written.String())
	}
	if geometry := engine.CurrentGeometry(); geometry.Rows != 33 || geometry.Columns != 99 {
		t.Fatalf("geometry = %+v", geometry)
	}
}
