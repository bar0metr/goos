package exec

import "testing"

// chunks returns a reader over parts, then end of file, then failed reads.
func chunks(parts ...string) func() ([]byte, bool) {
	index := 0
	return func() ([]byte, bool) {
		if index > len(parts) {
			return nil, false
		}
		index++
		if index > len(parts) {
			return nil, true
		}
		return []byte(parts[index-1]), true
	}
}

func TestCopyExecutableCopiesTheWholeFile(t *testing.T) {
	var object []byte
	write := func(data []byte) bool { object = append(object, data...); return true }
	if status := CopyExecutable(9, chunks("abc", "defg", "hi"), write); status != CopyOK || string(object) != "abcdefghi" {
		t.Fatalf("copy = %d, %q", status, object)
	}
}

func TestCopyExecutableRefusesAFileThatChanged(t *testing.T) {
	write := func([]byte) bool { return true }
	for _, test := range []struct {
		size  uint64
		parts []string
	}{
		{10, []string{"abc", "defg", "hi"}}, // shrank
		{8, []string{"abc", "defg", "hi"}},  // grew
		{2, []string{"abc"}},                // grew within one read
	} {
		if status := CopyExecutable(test.size, chunks(test.parts...), write); status != CopySizeChanged {
			t.Fatalf("size %d parts %q = %d", test.size, test.parts, status)
		}
	}
}

func TestCopyExecutableReportsFailures(t *testing.T) {
	failedRead := func() ([]byte, bool) { return nil, false }
	if status := CopyExecutable(3, failedRead, func([]byte) bool { return true }); status != CopyReadFailed {
		t.Fatalf("read failure = %d", status)
	}
	if status := CopyExecutable(3, chunks("abc"), func([]byte) bool { return false }); status != CopyWriteFailed {
		t.Fatalf("write failure = %d", status)
	}
}

func TestExecutableName(t *testing.T) {
	for path, want := range map[string]string{"/home/demo/args": "args", "/args": "args", "args": "args", "/": ""} {
		if got := ExecutableName([]byte(path)); string(got) != want {
			t.Fatalf("%q = %q", path, got)
		}
	}
}
