package main

import (
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
)

// listing builds `go tool objdump` output for one function from its
// instructions, one per line, at consecutive four-byte addresses from base.
// Branch targets are written as addresses, the way objdump prints them.
func listing(name string, base uint64, instructions ...string) string {
	var text strings.Builder
	fmt.Fprintf(&text, "TEXT %s(SB) \n", name)
	for index, instruction := range instructions {
		fmt.Fprintf(&text, "  :0\t\t\t%#x\t\t90909090\t\t%s\t\t\n", base+uint64(index)*4, instruction)
	}
	text.WriteString("\n")
	return text.String()
}

func analyzeListing(t *testing.T, text string) (int64, []string) {
	t.Helper()
	functions, err := parseObjdump(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	if len(functions) != 1 {
		t.Fatalf("parsed %d functions, want 1", len(functions))
	}
	return analyzeFunction(functions[0])
}

func TestParseObjdump(t *testing.T) {
	text := "TEXT _start(SB) \n" +
		"  :0\t\t\t0x40002000\t\t4883e4f0\t\tANDQ $-0x10, SP\t\t\n" +
		"  :0\t\t\t0x40002004\t\te8e60c0000\t\tCALL userspaceMain(SB)\t\n" +
		"\n" +
		"TEXT (goos/userspace/startup.Data).Handle(SB) /src/startup.go\n" +
		"  startup.go:12\t\t0x40003000\t\tc3\t\tRET\t\n"
	functions, err := parseObjdump(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	if len(functions) != 2 || functions[0].name != "_start" || functions[1].name != "(goos/userspace/startup.Data).Handle" {
		t.Fatalf("functions = %+v", functions)
	}
	if got := functions[0].instructions[1]; got.address != 0x40002004 || got.text != "CALL userspaceMain(SB)" {
		t.Fatalf("instruction = %+v", got)
	}
	for _, malformed := range []string{
		"  :0\t0x40002000\t90\tNOPL\n", // before any function
		"TEXT f(SB)\n  :0\t\tnot-an-address\t\t90\t\tNOPL\n",
		"TEXT f(SB)\n  :0\t\t0x40002000\t\t90\n",
		"TEXT broken\n",
	} {
		if _, err := parseObjdump(strings.NewReader(malformed)); err == nil {
			t.Errorf("accepted %q", malformed)
		}
	}
}

func TestFrameCountsPushesAllocationsAndArguments(t *testing.T) {
	frame, problems := analyzeListing(t, listing("main.run", 0x1000,
		"PUSHQ BP",
		"PUSHQ BX",
		"SUBQ $0x1aa8, SP",
		"PUSHL $0x2", // the -Oz idiom for loading a small constant
		"POPQ AX",
		"PUSHQ 0x10(SP)", // a stack argument
		"PUSHQ 0x18(SP)",
		"CALL main.send(SB)",
		"ADDQ $0x10, SP",
		"ADDQ $0x1aa8, SP",
		"POPQ BX",
		"POPQ BP",
		"RET",
	))
	if len(problems) != 0 || frame != 16+0x1aa8+16 {
		t.Fatalf("frame = %d, problems = %v", frame, problems)
	}
}

// Code after an early return still runs inside the frame; a scan that reset
// the depth at RET would miss the argument pushed there.
func TestFrameFollowsBranchesPastEarlyReturns(t *testing.T) {
	frame, problems := analyzeListing(t, listing("f", 0x1000,
		"PUSHQ BX",         // 0x1000
		"SUBQ $0x1000, SP", // 0x1004
		"CMPQ AX, $0x0",    // 0x1008
		"JE 0x1020",        // 0x100c
		"ADDQ $0x1000, SP", // 0x1010
		"POPQ BX",          // 0x1014
		"RET",              // 0x1018
		"UD2",              // 0x101c
		"PUSHQ AX",         // 0x1020
		"CALL g(SB)",       // 0x1024
		"ADDQ $0x8, SP",    // 0x1028
		"JMP 0x1010",       // 0x102c
	))
	if len(problems) != 0 || frame != 8+0x1000+8 {
		t.Fatalf("frame = %d, problems = %v", frame, problems)
	}
}

func TestFrameAcceptsNegativeImmediates(t *testing.T) {
	frame, problems := analyzeListing(t, listing("f", 0x1000,
		"ADDQ $-0x80, SP", // allocates 128 bytes
		"CALL g(SB)",
		"SUBQ $-0x80, SP", // releases them
		"RET",
	))
	if len(problems) != 0 || frame != 128 {
		t.Fatalf("frame = %d, problems = %v", frame, problems)
	}
}

func TestFrameOfStartRealignment(t *testing.T) {
	frame, problems := analyzeListing(t, listing("_start", 0x40002000,
		"ANDQ $-0x10, SP",
		"CALL userspaceMain(SB)",
		"UD2",
	))
	if len(problems) != 0 || frame != 8 {
		t.Fatalf("frame = %d, problems = %v", frame, problems)
	}
}

func TestFrameOfJumpTableTargets(t *testing.T) {
	frame, problems := analyzeListing(t, listing("f", 0x1000,
		"SUBQ $0x100, SP",      // 0x1000
		"JMP 0x40010000(AX*8)", // 0x1004: targets only the table knows
		"PUSHQ AX",             // 0x1008: one of them
		"CALL g(SB)",           // 0x100c
		"POPQ AX",              // 0x1010
		"ADDQ $0x100, SP",      // 0x1014
		"RET",                  // 0x1018
		"JMP 0x1008",           // 0x101c: another, falling into the first
	))
	if len(problems) != 0 || frame != 0x108 {
		t.Fatalf("frame = %d, problems = %v", frame, problems)
	}
}

func TestDynamicStackPointerChangesAreRejected(t *testing.T) {
	for _, instruction := range []string{
		"SUBQ AX, SP",
		"ADDQ CX, SP",
		"MOVQ AX, SP",
		"LEAQ -0x10(SP), SP",
		"LEAQ 0x10(BP), SP",
		"LEAVEQ",
		"POPQ SP",
		"XCHGQ AX, SP",
		"XCHGQ SP, AX",
		"CMOVQEQ AX, SP",
		"ANDQ $-0x1000, SP",
		"ANDQ AX, SP",
		"ORQ $0x10, SP",
		"MOVL AX, SP",
	} {
		_, problems := analyzeListing(t, listing("main.dynamic", 0x1000, instruction, "RET"))
		if len(problems) != 1 || !strings.HasPrefix(problems[0], "main.dynamic at 0x1000: ") {
			t.Errorf("%s: problems = %v", instruction, problems)
		}
	}
}

func TestStackPointerReadsAreAccepted(t *testing.T) {
	frame, problems := analyzeListing(t, listing("f", 0x1000,
		"MOVQ SP, BP",
		"LEAQ 0x8(SP), DI",
		"CMPQ SP, AX",
		"PUSHQ SP",
		"POPQ AX",
		"MOVQ AX, 0x10(SP)",
		"RET",
	))
	if len(problems) != 0 || frame != 8 {
		t.Fatalf("frame = %d, problems = %v", frame, problems)
	}
}

func TestRedZoneBound(t *testing.T) {
	if _, problems := analyzeListing(t, listing("leaf", 0x1000, "MOVUPS X0, -0x80(SP)", "RET")); len(problems) != 0 {
		t.Fatalf("red zone store rejected: %v", problems)
	}
	if _, problems := analyzeListing(t, listing("leaf", 0x1000, "MOVQ AX, -0x88(SP)", "RET")); len(problems) != 1 {
		t.Fatalf("store below the red zone accepted: %v", problems)
	}
}

func TestUnboundedDepthIsRejected(t *testing.T) {
	_, problems := analyzeListing(t, listing("f", 0x1000,
		"PUSHQ AX",   // 0x1000
		"JNE 0x1000", // 0x1004: pushes on every iteration
		"RET",
	))
	if len(problems) != 1 || !strings.Contains(problems[0], "does not converge") {
		t.Fatalf("problems = %v", problems)
	}
}

func TestUndecodableInstructionIsRejected(t *testing.T) {
	if _, problems := analyzeListing(t, listing("f", 0x1000, "?", "RET")); len(problems) != 1 {
		t.Fatalf("problems = %v", problems)
	}
}

func TestCheckFunctionsReportsTheLargestFrame(t *testing.T) {
	functions, err := parseObjdump(strings.NewReader(
		listing("small", 0x1000, "SUBQ $0x10, SP", "ADDQ $0x10, SP", "RET") +
			listing("large", 0x2000, "SUBQ $0x2000, SP", "ADDQ $0x2000, SP", "RET")))
	if err != nil {
		t.Fatal(err)
	}
	result := checkFunctions(functions, userFrameLimit)
	if len(result.problems) != 0 || result.largest != 0x2000 || result.function != "large" {
		t.Fatalf("report = %+v", result)
	}
	result = checkFunctions(functions, 0x1000)
	if len(result.problems) != 1 || !strings.HasPrefix(result.problems[0], "large: frame of 8192 bytes") ||
		!strings.Contains(result.summary(), "largest frame 8192 bytes in large") {
		t.Fatalf("report = %+v, summary %q", result, result.summary())
	}
}

func TestUserFrameLimit(t *testing.T) {
	if userFrameLimit != 64*1024-136 {
		t.Fatalf("userFrameLimit = %d", userFrameLimit)
	}
	// The fault probe's frame (tools/service-image) must be below the bound
	// it demonstrates.
	if 0x3000+returnAddressSize > userFrameLimit {
		t.Fatal("the fault probe's frame exceeds the bound")
	}
}

func probeCall(code []byte, at int, base, probe uint64, size uint32) {
	code[at] = 0xb8
	binary.LittleEndian.PutUint32(code[at+1:], size)
	code[at+5] = 0xe8
	binary.LittleEndian.PutUint32(code[at+6:], uint32(int32(int64(probe)-int64(base+uint64(at)+10))))
	copy(code[at+10:], []byte{0x48, 0x29, 0xc4})
}

func TestScanFrames(t *testing.T) {
	const base, probe = uint64(0x401000), uint64(0x400ff0)
	code := make([]byte, 64)
	copy(code[0:], []byte{0x48, 0x81, 0xec, 0x48, 0x0d, 0x00, 0x00}) // sub $0xd48, %rsp
	copy(code[7:], []byte{0x48, 0x83, 0xec, 0x28})                   // sub $0x28, %rsp
	probeCall(code, 16, base, probe, 0x1ac8)
	frames, problems := scanFrames(code, base, probe)
	want := []kernelFrame{{address: base, size: 0xd48}, {address: base + 16, size: 0x1ac8, probed: true}}
	if len(problems) != 0 || fmt.Sprint(frames) != fmt.Sprint(want) {
		t.Fatalf("frames = %v, problems = %v", frames, problems)
	}

	// A probe call that is not the allocation sequence cannot be bounded.
	odd := make([]byte, 32)
	probeCall(odd, 8, base, probe, 0x2000)
	odd[8] = 0x90
	if _, problems := scanFrames(odd, base, probe); len(problems) != 1 {
		t.Fatalf("problems = %v", problems)
	}
}

func TestCheckKernelFrames(t *testing.T) {
	symbols := []symbol{{"goos/kernel/kcall.Dispatch", 0x1000}, {"main.serviceKernel", 0x2000}, {"main.big", 0x3000}}
	result := checkKernelFrames([]kernelFrame{
		{address: 0x1010, size: 0xd48},
		{address: 0x2010, size: 0x1ac8, probed: true},
		{address: 0x3010, size: kernelFrameLimit},
	}, symbols)
	if len(result.problems) != 0 || result.largest != kernelFrameLimit || result.function != "main.big" {
		t.Fatalf("report = %+v", result)
	}
	result = checkKernelFrames([]kernelFrame{
		{address: 0x2010, size: bootStackFrameLimit + 8, probed: true},
		{address: 0x3010, size: kernelFrameLimit + 8, probed: true},
	}, symbols)
	if len(result.problems) != 2 || !strings.HasPrefix(result.problems[0], "main.serviceKernel: ") ||
		!strings.HasPrefix(result.problems[1], "main.big: probed frame of 8192 bytes") {
		t.Fatalf("problems = %v", result.problems)
	}
	if kernelFrameLimit != 8192-8 || bootStackFrameLimit != 32768-8 {
		t.Fatalf("kernelFrameLimit = %d, bootStackFrameLimit = %d", kernelFrameLimit, bootStackFrameLimit)
	}
	// A boot-stack function may be larger than any other kernel frame, up to
	// its own stack's guard.
	result = checkKernelFrames([]kernelFrame{{address: 0x2010, size: bootStackFrameLimit, probed: true}}, symbols)
	if len(result.problems) != 0 {
		t.Fatalf("problems = %v", result.problems)
	}
	if owner(symbols, 0x0fff) != "0xfff" || owner(symbols, 0x2fff) != "main.serviceKernel" {
		t.Fatal("owner lookup")
	}
}
