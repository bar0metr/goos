package amd64

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"goos/kernel/kcall"
)

func TestStepOverRunsKernelEntriesAndFlagsToABreakpoint(t *testing.T) {
	const rip = 0x40001000
	for _, test := range []struct {
		name string
		code []byte
		want uint64
	}{
		{"int 0x80", []byte{0xcd, 0x80}, rip + 2},
		{"int3", []byte{0xcc}, rip + 1},
		{"int1", []byte{0xf1}, rip + 1},
		{"into", []byte{0xce}, rip + 1},
		{"syscall", []byte{0x0f, 0x05}, rip + 2},
		{"sysenter", []byte{0x0f, 0x34}, rip + 2},
		{"pushfq", []byte{0x9c}, rip + 1},
		{"popfq", []byte{0x9d}, rip + 1},
		{"rex pushfq", []byte{0x48, 0x9c}, rip + 2},
		{"rex popfq", []byte{0x41, 0x9d}, rip + 2},
		{"o16 pushf", []byte{0x66, 0x9c}, rip + 2},
		{"o16 popf", []byte{0x66, 0x9d}, rip + 2},
		{"prefixed syscall", []byte{0x2e, 0x0f, 0x05}, rip + 3},
		{"prefixed int", []byte{0x66, 0x67, 0xcd, 0x80}, rip + 4},
		{"push r15", []byte{0x41, 0x57}, 0},
		{"call rel32", []byte{0xe8, 0x10, 0x00, 0x00, 0x00}, 0},
		{"ret imm16", []byte{0xc2, 0x80, 0x00}, 0},
		{"fxsave64", []byte{0x48, 0x0f, 0xae, 0x04, 0x24}, 0},
		{"ud2", []byte{0x0f, 0x0b}, 0},
		{"pushq imm8", []byte{0x6a, 0x00}, 0},
		{"mov ds", []byte{0x8e, 0xd8}, 0},
		{"one byte before an unmapped page", []byte{0x57}, 0},
	} {
		got, ok := StepOver(rip, test.code)
		if !ok || got != test.want {
			t.Errorf("%s: breakpoint %#x %v, want %#x", test.name, got, ok, test.want)
		}
	}
}

func TestStepOverRefusesTrapShadowsIretAndTruncatedCode(t *testing.T) {
	for _, test := range []struct {
		name string
		code []byte
	}{
		{"mov ss, ax", []byte{0x8e, 0xd0}},
		{"mov ss, mem", []byte{0x8e, 0x10}},
		{"o16 mov ss", []byte{0x66, 0x8e, 0xd0}},
		{"rex mov ss", []byte{0x48, 0x8e, 0xd0}},
		{"prefixed mov ss", []byte{0x2e, 0x3e, 0x8e, 0x17}},
		{"pop ss", []byte{0x17}},
		{"iretq", []byte{0x48, 0xcf}},
		{"iret", []byte{0xcf}},
		{"nothing", nil},
		{"prefixes only", []byte{0x66, 0x66, 0x66}},
		{"rex only", []byte{0x48}},
		{"int without vector", []byte{0xcd}},
		{"0f without second byte", []byte{0x0f}},
		{"8e without modrm", []byte{0x8e}},
		{"too many prefixes", []byte{0x66, 0x66, 0x66, 0x66, 0x66, 0x66, 0x66, 0x66, 0x66, 0x66, 0x66, 0x66, 0x66, 0x66, 0x66, 0x90}},
	} {
		if got, ok := StepOver(0x1000, test.code); ok {
			t.Errorf("%s: stepped to %#x", test.name, got)
		}
	}
}

func TestReturnIntoAcceptsOnlyACallInTheRange(t *testing.T) {
	const lo, hi = 0x1000, 0x1080
	for _, test := range []struct {
		ret, previous uint64
		want          bool
	}{
		{0x1045, 0x1040, true},                   // call rel32 at 0x1040
		{0x1042, 0x1040, true},                   // call *%rax
		{0x1040 + maxInstruction, 0x1040, true},  // longest call
		{0x1041 + maxInstruction, 0x1040, false}, // too far to be its return
		{0x1040, 0x1040, false},                  // not past the instruction
		{0x1030, 0x1040, false},                  // before it
		{0x1080, 0x107e, false},                  // past the range
		{0x0ff8, 0x0ff0, false},                  // below the range
		{0xdeadbeef, 0x107f, false},              // a ret's stale stack word
		{0x1003, 0x107f, false},                  // a ret's word inside the range
	} {
		if got := ReturnInto(test.ret, test.previous, lo, hi); got != test.want {
			t.Errorf("ReturnInto(%#x, %#x) = %v, want %v", test.ret, test.previous, got, test.want)
		}
	}
}

func TestHoldStubsUseAppendedOpcodes(t *testing.T) {
	wire, err := os.ReadFile("../../../userspace/runtime/amd64.S")
	if err != nil {
		t.Fatal(err)
	}
	for _, stub := range []struct {
		name      string
		operation kcall.Operation
	}{
		{"goos_thread_hold_range", kcall.OperationThreadHoldRange},
		{"goos_thread_hold_wait", kcall.OperationThreadHoldWait},
		{"goos_thread_hold_step", kcall.OperationThreadHoldStep},
		{"goos_thread_hold_cancel", kcall.OperationThreadHoldCancel},
	} {
		body := assemblyBody(t, string(wire), stub.name+":", ".size "+stub.name)
		match := regexp.MustCompile(`movl\s+\$(\d+),\s*%eax`).FindStringSubmatch(body)
		if len(match) != 2 || !strings.Contains(body, "int $0x80") {
			t.Fatalf("%s has no native call: %s", stub.name, body)
		}
		if opcode, err := strconv.ParseUint(match[1], 10, 64); err != nil || opcode != uint64(stub.operation) {
			t.Fatalf("%s opcode=%s, want %d", stub.name, match[1], stub.operation)
		}
	}
}

func TestDebugEntryHoldsWithoutSavingTheFPUAgain(t *testing.T) {
	wire, err := os.ReadFile("io.S")
	if err != nil {
		t.Fatal(err)
	}
	// The entry saves the FPU state before goos_user_debug, whose Go code
	// may use the SSE registers. The hold path must record the context and
	// switch without a second FXSAVE, and a task without a hold must reach
	// the fault path with vector 1 as before.
	entry := assemblyBody(t, string(wire), ".L_exception_1:", "GOOS_FATAL_NOERR 2")
	before, after, found := strings.Cut(entry, "GOOS_CALL goos_user_debug")
	if !found || !strings.Contains(before, "fxsave64") {
		t.Fatalf("debug entry does not save the FPU state before the handler:\n%s", entry)
	}
	hold := assemblyBody(t, after, ".debug_hold:", ".debug_terminate:")
	if strings.Contains(hold, "fxsave") || !strings.Contains(hold, "goos_task_contexts") || !strings.Contains(hold, "jmp .switch_away") {
		t.Fatalf("hold path:\n%s", hold)
	}
	fatal := assemblyBody(t, after, ".debug_fatal:", "jmp goos_exception_fatal")
	if !strings.Contains(fatal, "pushq $0") || !strings.Contains(fatal, "pushq $1") {
		t.Fatalf("debug fault path:\n%s", fatal)
	}
}

func TestStaleTrapResumesOnlyTheCancelledHoldsOwnException(t *testing.T) {
	const at = 0x40001000
	breakpoint := staleOf(&taskHold{state: holdRunning, dr0: at, dr7: dr7Local0})
	step := staleOf(&taskHold{state: holdRunning})
	standing := staleOf(&taskHold{state: holdStopped, dr0: at, dr7: dr7Local0})
	if breakpoint != (staleHold{breakpoint: at}) || step != (staleHold{step: true}) || standing != (staleHold{breakpoint: at}) {
		t.Fatalf("staleOf: breakpoint %+v step %+v standing %+v", breakpoint, step, standing)
	}
	for _, test := range []struct {
		name   string
		status uint64
		rip    uint64
		stale  staleHold
		want   bool
	}{
		{"breakpoint hit", dr6Clear | dr6B0, at, breakpoint, true},
		{"hit elsewhere", dr6Clear | dr6B0, at + 2, breakpoint, false},
		{"own step after breakpoint cancel", dr6Clear | dr6BS, at + 2, breakpoint, false},
		{"step in flight", dr6Clear | dr6BS, at + 3, step, true},
		{"breakpoint without one armed", dr6Clear | dr6B0, at, step, false},
		{"no hold", dr6Clear | dr6BS, at, staleHold{}, false},
		{"int1 or icebp", dr6Clear, at, breakpoint, false},
	} {
		if got := staleTrap(test.status, test.rip, test.stale); got != test.want {
			t.Errorf("%s: staleTrap = %v, want %v", test.name, got, test.want)
		}
	}
}

func TestEndHoldWakesAStoppedTaskStillOnItsProcessor(t *testing.T) {
	for _, test := range []struct {
		name  string
		state uint8
		onCPU bool
		wake  bool
		stale staleHold
	}{
		// The window between the debug exception and the blocking switch.
		{"stopped on cpu", holdStopped, true, true, staleHold{breakpoint: 0x1000}},
		{"left on cpu", holdLeft, true, true, staleHold{breakpoint: 0x1000}},
		{"stopped off cpu", holdStopped, false, true, staleHold{}},
		{"running breakpoint on cpu", holdRunning, true, false, staleHold{breakpoint: 0x1000}},
		{"running off cpu", holdRunning, false, false, staleHold{}},
	} {
		hold := &taskHold{armed: true, state: test.state, dr0: 0x1000, dr7: dr7Local0}
		stale, wake := endHold(hold, test.onCPU)
		if wake != test.wake || stale != test.stale {
			t.Errorf("%s: wake %v stale %+v, want %v %+v", test.name, wake, stale, test.wake, test.stale)
		}
	}
	stale, wake := endHold(&taskHold{armed: true, state: holdRunning}, true)
	if wake || stale != (staleHold{step: true}) {
		t.Fatalf("step in flight: wake %v stale %+v", wake, stale)
	}
}

func TestKernelDebugSingleStepResumesWithoutPanic(t *testing.T) {
	wire, err := os.ReadFile("io.S")
	if err != nil {
		t.Fatal(err)
	}
	// A step trap MOV SS delayed into the kernel's entry arrives in kernel
	// mode. The entry clears DR6 and TF and returns instead of panicking;
	// any other kernel-mode debug exception stays fatal.
	entry := assemblyBody(t, string(wire), ".L_exception_1:", ".debug_hold:")
	if !strings.Contains(entry, "jne .debug_kernel") {
		t.Fatalf("kernel-mode debug exceptions skip .debug_kernel:\n%s", entry)
	}
	path := assemblyBody(t, string(wire), ".debug_kernel:", "jmp .debug_fatal")
	for _, want := range []string{"movq %dr6, %rax", "$0x4000", "movq %rax, %dr6", "andq $-0x101, 24(%rsp)", "iretq"} {
		if !strings.Contains(path, want) {
			t.Fatalf("kernel debug path lacks %q:\n%s", want, path)
		}
	}
}

func TestArmHoldRefusesAnArmedHoldAndClearsResume(t *testing.T) {
	hold := &taskHold{}
	if !armHold(hold, 0x1000, 0x1010) || *hold != (taskHold{armed: true, state: holdRunning, lo: 0x1000, hi: 0x1010, dr0: 0x1000, dr7: dr7Local0}) {
		t.Fatalf("arm: %+v", hold)
	}
	stopped := &taskHold{armed: true, state: holdStopped, rip: 0x1004, dr0: 0x1004, dr7: dr7Local0}
	before := *stopped
	if armHold(stopped, 0x2000, 0x2010) || *stopped != before {
		t.Fatalf("re-armed a stopped hold: %+v", stopped)
	}
	// A cancelled hold's leftover does not block a new one.
	if !armHold(&taskHold{stale: staleHold{step: true}}, 0x1000, 0x1010) {
		t.Fatal("refused a cancelled hold")
	}
	if got := armFlags(0x202 | flagRF | flagTF); got != 0x202|flagTF {
		t.Fatalf("armFlags = %#x", got)
	}
}
