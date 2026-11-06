package goroutine

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"goos/userspace/runtime/tls"
)

// TestStaticTasksCoverEverySlot checks the TinyGo overlay's static task
// table against the indexes the scheduler uses: a scheduler stack and a
// thread.Start function per slot, and the main function.
func TestStaticTasksCoverEverySlot(t *testing.T) {
	source, err := os.ReadFile("../../../tinygo/goroutines/task_stack.go.txt")
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("var goosTasks [%d]Task", staticTasks); !strings.Contains(string(source), want) {
		t.Fatalf("the overlay does not declare %q", want)
	}
	if taskPinned+tls.Slots-1 >= staticTasks || taskMain >= taskPinned {
		t.Fatal("static task indexes overlap or overflow")
	}
}

// A process that never starts a second thread names only the main M; the
// other slots' Ms stay unreserved.
func TestOnlyStartedSlotsHaveMs(t *testing.T) {
	for slot := 1; slot < tls.Slots; slot++ {
		if mOf(slot) != nil {
			t.Fatalf("slot %d has an M before its thread ran", slot)
		}
	}
	if mRecords.At(1, 1) != nil {
		t.Fatal("the M records are reserved before a second thread")
	}
}
