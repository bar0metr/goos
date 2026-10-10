package tty

import (
	"bytes"
	"strings"
	"testing"
)

func TestGeometryProtocolIndependentFlagsAndTerm(t *testing.T) {
	if Version != 4 {
		t.Fatal("geometry payload changed without a version bump")
	}
	for _, display := range []bool{false, true} {
		for _, authoritative := range []bool{false, true} {
			geometry := Geometry{Columns: 1, Rows: 1000, Display: display, Authoritative: authoritative, Term: []byte("xterm-256color")}
			message, ok := GeometryStateReply(geometry, 1)
			if !ok {
				t.Fatal("geometry encode failed")
			}
			result, payload, ok := DecodeReply(&message)
			decoded, valid := DecodeGeometryState(payload)
			if !ok || !valid || result != ResultGeometry || decoded.Columns != 1 || decoded.Rows != 1000 ||
				decoded.Display != display || decoded.Authoritative != authoritative || !bytes.Equal(decoded.Term, geometry.Term) {
				t.Fatalf("decoded geometry %+v valid=%v", decoded, valid)
			}
			message.Version = 3
			if _, _, ok := DecodeReply(&message); ok {
				t.Fatal("v3 geometry decoded as v4")
			}
		}
	}
}

func TestGeometryRejectsMalformedPayloads(t *testing.T) {
	valid := []byte{80, 0, 25, 0, GeometryAuthoritative, 1, 'x'}
	for name, payload := range map[string][]byte{
		"v3":             {80, 0, 25, 0, GeometryDisplay},
		"unknown flags":  {80, 0, 25, 0, 4, 0},
		"zero columns":   {0, 0, 25, 0, 0, 0},
		"zero rows":      {80, 0, 0, 0, 0, 0},
		"short term":     {80, 0, 25, 0, 0, 2, 'x'},
		"trailing bytes": {80, 0, 25, 0, 0, 0, 'x'},
		"escape term":    {80, 0, 25, 0, 0, 1, 0x1b},
		"non-ascii term": {80, 0, 25, 0, 0, 1, 0xff},
		"oversized term": append([]byte{80, 0, 25, 0, 0, MaxTerm + 1}, bytes.Repeat([]byte{'x'}, MaxTerm+1)...),
	} {
		if _, ok := DecodeGeometryState(payload); ok {
			t.Fatalf("%s decoded", name)
		}
		if _, ok := Reply(ResultGeometry, payload, 1); ok {
			t.Fatalf("%s encoded", name)
		}
	}
	if _, ok := Reply(ResultGeometry, valid, 1); !ok {
		t.Fatal("valid payload refused")
	}
}

func TestRemoteGeometryBoundsAndCopiesTerm(t *testing.T) {
	h := newEngineHarness()
	term := []byte("vt100")
	if !h.engine.SetRemoteGeometry(1, 1, term) {
		t.Fatal("minimum remote geometry refused")
	}
	term[0] = 'X'
	h.request(t, OperationGeometry, "", 1)
	_, payload, ok := DecodeReply(&h.replies[0])
	geometry, valid := DecodeGeometryState(payload)
	if !ok || !valid || geometry.Display || !geometry.Authoritative || string(geometry.Term) != "vt100" || geometry.Columns != 1 || geometry.Rows != 1 {
		t.Fatalf("geometry %+v", geometry)
	}
	for _, dimensions := range [][2]int{{0, 1}, {1, 0}, {-1, 25}, {80, -1}, {1001, 1}, {1, 1001}} {
		if h.engine.SetRemoteGeometry(dimensions[0], dimensions[1], nil) {
			t.Fatalf("accepted %v", dimensions)
		}
	}
	for _, term := range []string{"\x00", "x\n", "\x1b[H", "\x7f", "\x80", strings.Repeat("x", MaxTerm+1)} {
		if h.engine.SetRemoteGeometry(80, 25, []byte(term)) {
			t.Fatalf("accepted TERM %q", term)
		}
	}
	if !h.engine.SetRemoteGeometry(1000, 1000, []byte(strings.Repeat("x", MaxTerm))) {
		t.Fatal("maximum remote geometry refused")
	}
	h.request(t, OperationGeometry, "", 2)
	_, payload, _ = DecodeReply(&h.replies[1])
	geometry, _ = DecodeGeometryState(payload)
	if geometry.Columns != 1000 || geometry.Rows != 1000 || len(geometry.Term) != MaxTerm {
		t.Fatalf("latest geometry %+v", geometry)
	}
}

func TestEngineResizeKeepsPendingLineAndRawOutput(t *testing.T) {
	h := newEngineHarness()
	h.request(t, OperationReadLine, "> ", 1)
	h.feed("pending")
	if !h.engine.SetRemoteGeometry(1, 1, []byte("vt100")) {
		t.Fatal("resize refused")
	}
	h.feed("\n")
	h.reply(t, 0, ResultLine, "pending", 1)
	h.request(t, OperationRawMode, "", 2)
	h.output.Reset()
	if !h.engine.SetRemoteGeometry(1000, 1000, []byte("xterm")) {
		t.Fatal("raw resize refused")
	}
	if h.output.Len() != 0 {
		t.Fatalf("resize wrote into raw screen: %q", h.output.String())
	}
}

func TestScreenSupportsNarrowRemoteGeometryWithoutOverflow(t *testing.T) {
	for _, rows := range []int{1, 1000, 65535} {
		var screen Screen
		screen.SetGeometry(1, rows)
		screen.Show(bytes.Repeat([]byte{'p'}, MaxPrompt), bytes.Repeat([]byte{'x'}, MaxLine))
		if columns, gotRows := screen.Geometry(); columns != 1 || gotRows != rows {
			t.Fatalf("geometry %dx%d", columns, gotRows)
		}
		if len(screen.Hide()) == 0 {
			t.Fatal("narrow line not hidden")
		}
	}
}
