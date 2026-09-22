package utility

import "testing"

func TestBenchCount(t *testing.T) {
	for _, test := range []struct {
		value string
		want  uint64
		ok    bool
	}{
		{"1", 1, true},
		{"200", 200, true},
		{"10000", MaxBenchOps, true},
		{"0", 0, false},
		{"10001", 0, false},
		{"", 0, false},
		{"-5", 0, false},
		{"12a", 0, false},
		{"99999999999999999999999", 0, false},
	} {
		count, ok := benchCount([]byte(test.value))
		if ok != test.ok || ok && count != test.want {
			t.Fatalf("benchCount(%q) = %d, %v", test.value, count, ok)
		}
	}
}

func TestAppendBenchResult(t *testing.T) {
	var storage [128]byte
	for _, test := range []struct {
		name       string
		ops, nanos uint64
		want       string
	}{
		{"vfs", 200, 12_345_678, "bench vfs: 200 ops, total 12345 us, avg 61728 ns/op"},
		{"read", 1, 999, "bench read: 1 ops, total 0 us, avg 999 ns/op"},
		{"vfs", 0, 0, "bench vfs: 0 ops, total 0 us, avg 0 ns/op"},
		{"vfs", MaxBenchOps, ^uint64(0), "bench vfs: 10000 ops, total 18446744073709551 us, avg 1844674407370955 ns/op"},
	} {
		got := appendBenchResult(storage[:0], test.name, test.ops, test.nanos)
		if string(got) != test.want {
			t.Fatalf("got %q, want %q", got, test.want)
		}
		if len(got) > len(storage) {
			t.Fatalf("result outgrew its fixed buffer: %d bytes", len(got))
		}
	}
}
