package log

import (
	"testing"

	"goos/internal/logfmt"
)

func TestCounterLogFits(t *testing.T) {
	for _, message := range []string{"counters", "prefetch"} {
		var fields [logfmt.MaxFields]byte
		encoded := fields[:0]
		var ok bool
		if message == "counters" {
			encoded, ok = logfmt.AppendField(encoded, "cache-reads", []byte("123456"))
			if ok {
				encoded, ok = logfmt.AppendField(encoded, "cache-hits", []byte("123456"))
			}
		} else {
			encoded, ok = logfmt.AppendField(encoded, "reads", []byte("123456"))
			if ok {
				encoded, ok = logfmt.AppendField(encoded, "fetches", []byte("123456"))
			}
		}
		if !ok {
			t.Fatalf("%s: fields too long", message)
		}
		var wire [logfmt.MaxWire]byte
		if _, ok := encode(logfmt.Warn, "ext4", message, []Field{{Key: "cache-reads", Uint: 123456, Kind: kindUint}}, &wire); !ok {
			t.Fatalf("%s: encode failed", message)
		}
	}
}
