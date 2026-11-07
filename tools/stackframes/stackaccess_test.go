package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// encodedListing is listing with real machine code: each instruction is
// "encoding\ttext".
func encodedListing(name string, base uint64, instructions ...string) string {
	var text strings.Builder
	fmt.Fprintf(&text, "TEXT %s(SB) \n", name)
	for index, instruction := range instructions {
		encoding, assembly, _ := strings.Cut(instruction, "\t")
		fmt.Fprintf(&text, "  :0\t\t\t%#x\t\t%s\t\t%s\t\t\n", base+uint64(index)*8, encoding, assembly)
	}
	return text.String() + "\n"
}

func parseOne(t *testing.T, text string) function {
	t.Helper()
	functions, err := parseObjdump(strings.NewReader(text))
	if err != nil || len(functions) != 1 {
		t.Fatalf("parse: %v (%d functions)", err, len(functions))
	}
	return functions[0]
}

func sha256Block(t *testing.T) function {
	t.Helper()
	data, err := os.ReadFile("testdata/sha256-block.objdump")
	if err != nil {
		t.Fatal(err)
	}
	return parseOne(t, string(data))
}

// The fixture is crypto/internal/fips140/sha256.block from the sha256 and
// ecdsa crypto probes built with the compatibility root. The checker that
// first decoded objdump's unsigned displacements rejected two of its
// instructions as reaching 140 and 144 bytes below SP. They index the
// message schedule with AX, which the loop holds at 16..63, so neither
// reaches below SP-80.
func TestSHA256BlockEvidence(t *testing.T) {
	fn := sha256Block(t)
	if fn.name != "crypto/internal/fips140/sha256.block" {
		t.Fatalf("fixture holds %s", fn.name)
	}
	want := map[string]int64{
		"MOVL 0xffffff74(SP)(AX*4), SI": -140,
		"ADDL 0xffffff70(SP)(AX*4), CX": -144,
	}
	for _, ins := range fn.instructions {
		if strings.HasPrefix(ins.text, "CALL") {
			t.Fatalf("sha256.block calls: %s", ins.text)
		}
		if disp, ok := want[ins.text]; ok {
			accesses := decode(ins.text).accesses
			if len(accesses) != 1 || accesses[0].disp != disp || accesses[0].index != "AX" || accesses[0].scale != 4 {
				t.Fatalf("%s decodes to %+v", ins.text, accesses)
			}
			delete(want, ins.text)
		}
	}
	if len(want) != 0 {
		t.Fatalf("fixture lacks %v", want)
	}
	if preemptionActive([]function{fn}) {
		t.Fatal("the probe image has no goroutine preemption")
	}
	frame, _, _, problems := depths(fn)
	if len(problems) != 0 || frame != 6*8+0xb8 {
		t.Fatalf("frame %d, problems %v; want six pushes and SUBQ $0xb8", frame, problems)
	}
	result := analyze(fn, true)
	if len(result.problems) != 0 {
		t.Fatalf("sha256.block rejected even under preemption rules: %v", result.problems)
	}
	// The deepest access is MOVL R15, -0x78(SP), 120 bytes below the
	// 232-byte frame; the indexed loads reach only SP-80.
	if result.reach != frame+120 {
		t.Fatalf("reach %d, want %d", result.reach, frame+120)
	}
}

// Started at zero, the same loop would read 144 bytes below SP.
func TestSHA256BlockFromZeroNeedsTheLeafRule(t *testing.T) {
	fn := sha256Block(t)
	for i, ins := range fn.instructions {
		if ins.text == "MOVL $0x10, AX" {
			fn.instructions[i] = instruction{address: ins.address, encoding: "b800000000", text: "MOVL $0x0, AX"}
		}
	}
	if result := analyze(fn, true); len(result.problems) == 0 {
		t.Fatal("an access 144 bytes below SP passed in a preemptible image")
	}
	result := analyze(fn, false)
	if len(result.problems) != 0 {
		t.Fatalf("scheduler-none leaf rejected: %v", result.problems)
	}
	if want := int64(6*8+0xb8) + 144; result.reach != want {
		t.Fatalf("reach %d, want %d", result.reach, want)
	}
}

func TestBelowRedZoneLeafCountsInReach(t *testing.T) {
	fn := parseOne(t, listing("leaf", 0x1000, "SUBQ $0x20, SP", "MOVQ AX, -0x100(SP)", "ADDQ $0x20, SP", "RET"))
	result := analyze(fn, false)
	if len(result.problems) != 0 || result.reach != 0x20+0x100 {
		t.Fatalf("reach %d, problems %v; want %d", result.reach, result.problems, 0x20+0x100)
	}
	if result := analyze(fn, true); len(result.problems) == 0 {
		t.Fatal("a preemptible image accepted storage below SP-128")
	}
	calls := parseOne(t, listing("caller", 0x1000, "MOVQ AX, -0x100(SP)", "CALL f(SB)", "MOVQ -0x100(SP), AX", "RET"))
	if result := analyze(calls, false); len(result.problems) == 0 {
		t.Fatal("a function that calls kept data below SP-128")
	}
	tail := parseOne(t, listing("tail", 0x1000, "MOVQ AX, -0x100(SP)", "JMP f(SB)"))
	if result := analyze(tail, false); len(result.problems) != 0 {
		t.Fatalf("a tail call rejected: %v", result.problems)
	}
}

func TestReachThatSkipsTheGuardIsRejected(t *testing.T) {
	limit := userFrameLimit
	frame := fmt.Sprintf("SUBQ $%#x, SP", limit-0x100)
	fits := parseOne(t, listing("fits", 0x1000, frame, "MOVQ AX, -0x100(SP)", "RET"))
	skips := parseOne(t, listing("skips", 0x1000, frame, "MOVQ AX, 0xfffff000(SP)", "RET"))
	result := checkFunctions([]function{fits}, limit)
	if len(result.problems) != 0 {
		t.Fatalf("an access inside the guard rejected: %v", result.problems)
	}
	result = checkFunctions([]function{skips}, limit)
	if len(result.problems) != 1 || !strings.Contains(result.problems[0], "past") {
		t.Fatalf("an access past the guard accepted: %v", result.problems)
	}
}

// The tool clears an indexed base below the red zone only with a proved
// index; inside it, the index is the compiler's to bound, and the base
// counts in the reach.
func TestIndexBoundary(t *testing.T) {
	for _, text := range []string{"MOVL 0xffffff70(SP)(AX*4), CX", "MOVL -0x90(SP)(AX*4), CX"} {
		fn := parseOne(t, listing("index", 0x1000, text, "RET"))
		if result := analyze(fn, false); len(result.problems) == 0 {
			t.Fatalf("%s with an unknown AX accepted", text)
		}
	}
	for text, reach := range map[string]int64{
		"MOVQ 0(SP)(AX*8), CX":     0x10,
		"MOVL 0x40(SP)(AX*4), CX":  0x10,
		"MOVB CL, -0x7f(SP)(DX*1)": 0x10 + 0x7f,
		"MOVL -0x80(SP)(AX*4), CX": 0x10 + 0x80,
	} {
		fn := parseOne(t, listing("index", 0x1000, "SUBQ $0x10, SP", text, "ADDQ $0x10, SP", "RET"))
		result := analyze(fn, true)
		if len(result.problems) != 0 || result.reach != reach {
			t.Fatalf("%s: reach %d, problems %v; want reach %d", text, result.reach, result.problems, reach)
		}
	}
	for _, text := range []string{"MOVQ AX, 0(SP)(SP*1)", "MOVQ AX, (SP)", "MOVQ AX, 0x10(SP)(AX*3)"} {
		fn := parseOne(t, listing("odd", 0x1000, text, "RET"))
		if result := analyze(fn, false); len(result.problems) == 0 {
			t.Fatalf("%q accepted", text)
		}
	}
}

// loop is the second sha256.block loop, shortened, with real encodings.
func loop(init, body string) string {
	return encodedListing("loop", 0x1000,
		init,
		"8b4c84a8\tMOVL -0x58(SP)(AX*4), CX",
		body,
		"038c8470ffffff\tADDL 0xffffff70(SP)(AX*4), CX",
		"48ffc0\tINCQ AX",
		"4883f840\tCMPQ AX, $0x40",
		"75b5\tJNE 0x1008",
		"c3\tRET")
}

func TestCountedLoopProof(t *testing.T) {
	good := parseOne(t, loop("b810000000\tMOVL $0x10, AX", "c1c20f\tROLL $0xf, DX"))
	if result := analyze(good, true); len(result.problems) != 0 {
		t.Fatalf("the counted loop rejected: %v", result.problems)
	}
	for name, mutated := range map[string]string{
		"counter written in the body": loop("b810000000\tMOVL $0x10, AX", "4801c0\tADDQ AX, AX"),
		"byte write to the counter":   loop("b810000000\tMOVL $0x10, AX", "b001\tMOVB $0x1, AL"),
		"call in the body":            loop("b810000000\tMOVL $0x10, AX", "e800000000\tCALL f(SB)"),
		"initial value from memory":   loop("488b06\tMOVQ 0(SI), AX", "c1c20f\tROLL $0xf, DX"),
	} {
		if result := analyze(parseOne(t, mutated), false); len(result.problems) == 0 {
			t.Fatalf("%s: loop accepted", name)
		}
	}
	// The machine code, not objdump's text, gives the start: zero here,
	// so the ADDL reaches 144 bytes below SP.
	bytesSayZero := parseOne(t, loop("b800000000\tMOVL $0x10, AX", "c1c20f\tROLL $0xf, DX"))
	if result := analyze(bytesSayZero, true); len(result.problems) == 0 {
		t.Fatal("the loop took its start from the text")
	}
	entered := encodedListing("entered", 0x1000,
		"7406\tJE 0x1010",
		"b810000000\tMOVL $0x10, AX",
		"8b4c84a8\tMOVL -0x58(SP)(AX*4), CX",
		"038c8470ffffff\tADDL 0xffffff70(SP)(AX*4), CX",
		"48ffc0\tINCQ AX",
		"4883f840\tCMPQ AX, $0x40",
		"75b5\tJNE 0x1010",
		"c3\tRET")
	if result := analyze(parseOne(t, entered), false); len(result.problems) == 0 {
		t.Fatal("a loop entered past its counter's initialisation accepted")
	}
}

func TestPreemptionDetection(t *testing.T) {
	entry := func(call string) function {
		return function{name: "goos_preempt_entry", instructions: []instruction{{text: call}}}
	}
	if !preemptionActive([]function{entry("CALL goos_g_preempt(SB)")}) {
		t.Fatal("a registered preemption entry not detected")
	}
	if preemptionActive([]function{entry("CALL 0x0")}) || preemptionActive(nil) {
		t.Fatal("a scheduler-none image detected as preemptible")
	}
	below := parseOne(t, listing("leaf", 0x1000, "MOVQ AX, -0x100(SP)", "RET"))
	if result := checkFunctions([]function{below, entry("CALL goos_g_preempt(SB)")}, userFrameLimit); len(result.problems) == 0 {
		t.Fatal("storage below SP-128 accepted beside a registered preemption entry")
	}
}
