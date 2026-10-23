package goroutine

import (
	"os"
	"strings"
	"testing"
)

// preemptEntryBody returns the instructions of goos_preempt_entry in
// amd64.S, one per element, without comments or labels.
func preemptEntryBody(t *testing.T) []string {
	t.Helper()
	source, err := os.ReadFile("../amd64.S")
	if err != nil {
		t.Fatal(err)
	}
	var body []string
	inside := false
	for _, line := range strings.Split(string(source), "\n") {
		if index := strings.Index(line, "//"); index >= 0 {
			line = line[:index]
		}
		line = strings.TrimSpace(line)
		switch {
		case line == "goos_preempt_entry:":
			inside = true
		case strings.HasPrefix(line, ".size goos_preempt_entry,"):
			return body
		case inside && line != "" && !strings.HasSuffix(line, ":") && !strings.HasPrefix(line, "."):
			body = append(body, line)
		}
	}
	t.Fatal("goos_preempt_entry not found in amd64.S")
	return nil
}

// TestPreemptEntrySavesVectorsBeforeKernelCall checks the order that keeps
// the interrupted code's XMM registers: a native call returns only the
// general-purpose registers, and the kernel's own code uses SSE, so the
// FPU and SSE state must be saved before the entry takes its RIP with
// ThreadPreemptTake and restored only after goos_g_preempt.
func TestPreemptEntrySavesVectorsBeforeKernelCall(t *testing.T) {
	body := preemptEntryBody(t)
	position := func(prefix string) int {
		for index, line := range body {
			if strings.HasPrefix(line, prefix) {
				return index
			}
		}
		t.Fatalf("goos_preempt_entry has no %q", prefix)
		return -1
	}
	save, call, visit, restore := position("fxsave64"), position("int $0x80"), position("call goos_g_preempt"), position("fxrstor64")
	if save > call {
		t.Fatalf("fxsave64 at instruction %d follows the kernel call at %d: the call clobbers live XMM registers", save, call)
	}
	if restore < visit {
		t.Fatalf("fxrstor64 at instruction %d precedes goos_g_preempt at %d", restore, visit)
	}
	for _, line := range body[:save] {
		if strings.Contains(line, "xmm") || strings.HasPrefix(line, "call") || strings.HasPrefix(line, "int") {
			t.Fatalf("%q runs before the SSE state is saved", line)
		}
	}
}
