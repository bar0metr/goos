package amd64

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"goos/kernel/kcall"
)

func TestLegacyIRQWaitIgnoresUnusedArgumentRegisters(t *testing.T) {
	wire, err := os.ReadFile("io.S")
	if err != nil {
		t.Fatal(err)
	}
	// Old IRQWait stubs leave RSI (Arg1) untouched. The special blocking
	// entry may copy only Arg0 from the user register frame; interpreting
	// Arg1 would reject old callers or accidentally turn their wait into a
	// poll. Verify the assembly boundary, which host tests cannot execute.
	body := assemblyBody(t, string(wire), ".user_call_irq_wait:", ".user_call_stream_wait:")
	loads := regexp.MustCompile(`movq\s+(\d+)\(%rsp\),`).FindAllStringSubmatch(body, -1)
	if len(loads) != 1 || loads[0][1] != "96" || !strings.Contains(body, "callq goos_dispatch_irq_wait") {
		t.Fatalf("IRQWait reads more than its legacy handle argument:\n%s", body)
	}
}

func TestIRQPollStubUsesAppendedNonblockingOpcode(t *testing.T) {
	wire, err := os.ReadFile("../../../userspace/runtime/amd64.S")
	if err != nil {
		t.Fatal(err)
	}
	body := assemblyBody(t, string(wire), "goos_irq_poll:", ".size goos_irq_poll")
	match := regexp.MustCompile(`movl\s+\$(\d+),\s*%eax`).FindStringSubmatch(body)
	if len(match) != 2 {
		t.Fatalf("IRQPoll has no opcode: %s", body)
	}
	opcode, err := strconv.ParseUint(match[1], 10, 64)
	if err != nil || opcode != uint64(kcall.OperationIRQPoll) || !strings.Contains(body, "int $0x80") {
		t.Fatalf("IRQPoll stub opcode=%d, want %d", opcode, kcall.OperationIRQPoll)
	}
}

func assemblyBody(t *testing.T, source, start, end string) string {
	t.Helper()
	_, after, found := strings.Cut(source, start)
	if !found {
		t.Fatalf("missing assembly label %s", start)
	}
	body, _, found := strings.Cut(after, end)
	if !found {
		t.Fatalf("missing assembly boundary %s", end)
	}
	return body
}
