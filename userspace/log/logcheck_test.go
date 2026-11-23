package log

import (
	"testing"

	"goos/internal/logfmt"
)

// TestCounterLogFits proves the two records the ext4 service's logCounters
// emits fit the wire format even at the widest uint64 values. The field keys
// and order match logCounters exactly: no second, fake encoding path.
func TestCounterLogFits(t *testing.T) {
	const maxValue = ^uint64(0)
	records := []struct {
		message string
		fields  []Field
	}{
		{"counters", []Field{
			{Key: "cache-reads", Uint: maxValue, Kind: kindUint},
			{Key: "cache-hits", Uint: maxValue, Kind: kindUint},
		}},
		{"prefetch", []Field{
			{Key: "reads", Uint: maxValue, Kind: kindUint},
			{Key: "fetches", Uint: maxValue, Kind: kindUint},
		}},
	}
	for _, record := range records {
		var wire [logfmt.MaxWire]byte
		if _, ok := encode(logfmt.Warn, "ext4", record.message, record.fields, &wire); !ok {
			t.Fatalf("%q does not fit the wire format", record.message)
		}
	}
}
