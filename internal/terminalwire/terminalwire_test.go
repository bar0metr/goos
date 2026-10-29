package terminalwire

import "testing"

func TestRecordRoundTrip(t *testing.T) {
	var record [RecordSize]byte
	if !Encode(Geometry{Rows: 50, Columns: 132, Sequence: 7}, &record) {
		t.Fatal("encode refused")
	}
	geometry, ok := Decode(record[:])
	if !ok || geometry != (Geometry{Rows: 50, Columns: 132, Sequence: 7}) {
		t.Fatalf("decoded %+v %v", geometry, ok)
	}
	record[2] = Version + 1
	if _, ok := Decode(record[:]); ok {
		t.Fatal("another version accepted")
	}
	if Encode(Geometry{Columns: 80}, &record) {
		t.Fatal("zero rows accepted")
	}
	if _, ok := Decode(record[:8]); ok {
		t.Fatal("short record accepted")
	}
}
