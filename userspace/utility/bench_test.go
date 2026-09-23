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
		count, ok := BenchCount([]byte(test.value))
		if ok != test.ok || ok && count != test.want {
			t.Fatalf("BenchCount(%q) = %d, %v", test.value, count, ok)
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
		got := AppendBenchResult(storage[:0], test.name, test.ops, test.nanos)
		if string(got) != test.want {
			t.Fatalf("got %q, want %q", got, test.want)
		}
		if len(got) > len(storage) {
			t.Fatalf("result outgrew its fixed buffer: %d bytes", len(got))
		}
	}
}

func TestBenchChunk(t *testing.T) {
	for _, test := range []struct {
		value string
		want  int
		ok    bool
	}{
		{"1", 1, true},
		{"256", MaxBenchChunk, true},
		{"0", 0, false},
		{"257", 0, false},
		{"", 0, false},
		{"x", 0, false},
	} {
		size, ok := BenchChunk([]byte(test.value))
		if ok != test.ok || ok && size != test.want {
			t.Fatalf("BenchChunk(%q) = %d, %v", test.value, size, ok)
		}
	}
}

func TestMedian(t *testing.T) {
	for _, test := range []struct {
		values []uint64
		want   uint64
	}{
		{nil, 0},
		{[]uint64{7}, 7},
		{[]uint64{9, 1, 5}, 5},
		{[]uint64{4, 3, 2, 1}, 2},
		{[]uint64{5, 5, 1, 5}, 5},
	} {
		values := append([]uint64(nil), test.values...)
		if got := Median(values); got != test.want {
			t.Fatalf("Median(%v) = %d, want %d", test.values, got, test.want)
		}
		for index := 1; index < len(values); index++ {
			if values[index-1] > values[index] {
				t.Fatalf("Median(%v) left %v unsorted", test.values, values)
			}
		}
	}
}

func TestAppendBenchRateAndCount(t *testing.T) {
	var storage [160]byte
	line := AppendBenchResult(storage[:0], "recv", 1024, 1_000_000_000)
	line = AppendBenchRate(line, 1024, 256, 1_000_000_000)
	line = AppendBenchCount(line, 1023, "waits")
	want := "bench recv: 1024 ops, total 1000000 us, avg 976562 ns/op, 256 B/op, 256 KiB/s, 1023 waits"
	if string(line) != want {
		t.Fatalf("got %q, want %q", line, want)
	}
	if got := string(AppendBenchRate(storage[:0], 5, 1, 0)); got != ", 1 B/op, 0 KiB/s" {
		t.Fatalf("zero duration: got %q", got)
	}
	// The largest run the bounds allow must not overflow.
	if got := string(AppendBenchRate(storage[:0], MaxBenchOps, MaxBenchChunk, 1_000_000_000)); got != ", 256 B/op, 2500 KiB/s" {
		t.Fatalf("largest run: got %q", got)
	}
	if got := string(AppendBenchRate(storage[:0], MaxBenchOps, 1<<40, 1_000_000_000)); got != ", 1099511627776 B/op, 10737418240000 KiB/s" {
		t.Fatalf("large file: got %q", got)
	}
	if got := string(AppendBenchRate(storage[:0], 1<<40, 1<<20, 1)); got != ", 1048576 B/op, 18446744073709551615 KiB/s" {
		t.Fatalf("saturation: got %q", got)
	}
}
