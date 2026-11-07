package main

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"goos/kernel/loader"
)

// Why a frame bound makes the userspace guard sound (ADR 0088):
//
// Every push and every call writes at the stack pointer it has just lowered,
// so a function is entered with its return address written at its entry stack
// pointer. By induction from _start, whose stack pointer the loader places
// inside the stack segment, that entry stack pointer is at or above the bottom
// of the stack, unless an earlier write had already faulted. Inside the
// function the stack pointer never goes more than the function's static depth
// (pushes, SUBQ immediates and realignment, computed below) below it, and no
// access goes more than the red zone below the stack pointer. So when every
// depth plus the margin fits in the guard, the first access below the stack
// lands in the guard and faults, whatever the call chain.
const (
	// returnAddressSize is what a call writes below the caller's stack pointer.
	returnAddressSize = 8
	// redZoneSize is how far below the stack pointer System V code may access
	// memory without moving it. TinyGo compiles Go code with the red zone:
	// the target's -mno-red-zone reaches only C sources, and Go functions do
	// store below SP. The tool also rejects any access further below.
	redZoneSize = 128
	// userMargin covers both. The return address lies inside the red zone,
	// so the red zone alone would do; the sum is simpler to argue.
	userMargin = returnAddressSize + redZoneSize
	// userFrameLimit is the largest static depth a userspace function may
	// have.
	userFrameLimit = int64(loader.StackGuardSize) - userMargin
	// maxAlignment is the largest stack realignment (ANDQ $-n, SP) accepted.
	// Realigning can lower SP by at most n-1 bytes.
	maxAlignment = 64
	// divergenceLimit stops the analysis of a function whose depth keeps
	// growing, which only a loop that allocates on the stack can cause.
	divergenceLimit = int64(1) << 24
)

type instruction struct {
	address  uint64
	encoding string // machine code in hex, as go tool objdump prints it
	text     string
}

type function struct {
	name         string
	instructions []instruction
}

// parseObjdump reads the output of `go tool objdump`: a "TEXT name(SB) file"
// line per function followed by one tab-separated line per instruction
// (position, address, encoding, Go-syntax assembly).
func parseObjdump(input io.Reader) ([]function, error) {
	var functions []function
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for line := 1; scanner.Scan(); line++ {
		text := scanner.Text()
		switch {
		case strings.TrimSpace(text) == "":
		case strings.HasPrefix(text, "TEXT "):
			name := strings.TrimPrefix(text, "TEXT ")
			end := strings.LastIndex(name, "(SB)")
			if end <= 0 {
				return nil, fmt.Errorf("objdump line %d: malformed function header %q", line, text)
			}
			functions = append(functions, function{name: name[:end]})
		default:
			fields := instructionFields(text)
			if len(functions) == 0 || len(fields) < 4 {
				return nil, fmt.Errorf("objdump line %d: malformed instruction %q", line, text)
			}
			address, err := strconv.ParseUint(fields[1], 0, 64)
			if err != nil {
				return nil, fmt.Errorf("objdump line %d: malformed address %q", line, fields[1])
			}
			last := &functions[len(functions)-1]
			last.instructions = append(last.instructions, instruction{address: address, encoding: fields[2], text: fields[3]})
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return functions, nil
}

func instructionFields(line string) []string {
	var fields []string
	for _, field := range strings.Split(line, "\t") {
		if field = strings.TrimSpace(field); field != "" {
			fields = append(fields, field)
		}
	}
	return fields
}

type flow int

const (
	flowNext     flow = iota // continues with the next instruction
	flowJump                 // continues only at target
	flowBranch               // continues at target or with the next instruction
	flowStop                 // leaves the function or traps
	flowIndirect             // jumps through a register or table
)

// step is what one instruction does to the stack depth and to control flow,
// and which memory it addresses relative to SP.
type step struct {
	delta    int64
	flow     flow
	target   uint64
	call     bool
	accesses []stackAccess
	problem  string
}

// stackAccess is a memory operand based on SP: disp(SP), or
// disp(SP)(index*scale) when index is set.
type stackAccess struct {
	disp  int64
	index string
	scale int64
}

// stackPointerOffset matches a memory operand that is SP plus a constant and
// nothing else.
var stackPointerOffset = regexp.MustCompile(`^(-?0x[0-9a-f]+|-?[0-9]+)\(SP\)$`)

// stackPointerIndexed matches SP plus a constant plus a scaled register.
var stackPointerIndexed = regexp.MustCompile(`^(-?0x[0-9a-f]+|-?[0-9]+)\(SP\)\(([A-Z0-9]+)\*([1248])\)$`)

// displacement parses the constant of an SP-relative operand. go tool
// objdump prints a large negative 32-bit displacement unsigned, so -4096 reads
// as 0xfffff000; such a value is the sign-extended displacement the
// instruction uses.
func displacement(text string) (int64, error) {
	value, err := strconv.ParseInt(text, 0, 64)
	if err == nil && value >= 1<<31 && value < 1<<32 {
		value -= 1 << 32
	}
	return value, err
}

func decode(text string) step {
	mnemonic, rest, _ := strings.Cut(text, " ")
	var operands []string
	if rest = strings.TrimSpace(rest); rest != "" {
		operands = strings.Split(rest, ", ")
	}
	if mnemonic == "?" {
		return step{problem: "undecodable instruction"}
	}
	var accesses []stackAccess
	for _, operand := range operands {
		if !strings.Contains(operand, "(SP)") {
			continue
		}
		access, ok := stackOperand(operand)
		if !ok {
			return step{problem: fmt.Sprintf("%q addresses the stack in a form the tool does not bound", text)}
		}
		accesses = append(accesses, access)
	}
	if writesStackPointer(mnemonic, operands) {
		return adjustment(text, mnemonic, operands)
	}
	result := decodeFlow(text, mnemonic, operands)
	result.accesses = accesses
	result.call = mnemonic == "CALL"
	return result
}

// stackOperand parses disp(SP) and disp(SP)(index*scale).
func stackOperand(operand string) (stackAccess, bool) {
	if match := stackPointerOffset.FindStringSubmatch(operand); match != nil {
		disp, err := displacement(match[1])
		return stackAccess{disp: disp}, err == nil
	}
	if match := stackPointerIndexed.FindStringSubmatch(operand); match != nil && match[2] != "SP" {
		disp, err := displacement(match[1])
		scale, _ := strconv.ParseInt(match[3], 10, 64)
		return stackAccess{disp: disp, index: match[2], scale: scale}, err == nil
	}
	return stackAccess{}, false
}

func decodeFlow(text, mnemonic string, operands []string) step {
	switch mnemonic {
	case "PUSHQ", "PUSHL", "PUSHFQ":
		// PUSHL is Go's name for pushing an immediate, eight bytes in
		// 64-bit mode.
		return step{delta: 8}
	case "PUSHW", "PUSHFW":
		return step{delta: 2}
	case "POPQ", "POPFQ":
		return step{delta: -8}
	case "POPW", "POPFW":
		return step{delta: -2}
	case "UD2", "HLT", "IRETQ":
		return step{flow: flowStop}
	case "INT":
		if len(operands) == 1 && operands[0] == "$0x3" {
			return step{flow: flowStop}
		}
		return step{}
	case "JMP":
		if len(operands) != 1 {
			return step{problem: fmt.Sprintf("malformed jump %q", text)}
		}
		if target, err := strconv.ParseUint(operands[0], 0, 64); err == nil {
			return step{flow: flowJump, target: target}
		}
		if strings.HasSuffix(operands[0], "(SB)") {
			return step{flow: flowStop} // tail call
		}
		return step{flow: flowIndirect}
	}
	if strings.HasPrefix(mnemonic, "PUSH") || strings.HasPrefix(mnemonic, "POP") && !strings.HasPrefix(mnemonic, "POPCNT") {
		return step{problem: fmt.Sprintf("unknown stack operation %q", text)}
	}
	if strings.HasPrefix(mnemonic, "RET") {
		return step{flow: flowStop}
	}
	if strings.HasPrefix(mnemonic, "J") || strings.HasPrefix(mnemonic, "LOOP") {
		if len(operands) != 1 {
			return step{problem: fmt.Sprintf("malformed branch %q", text)}
		}
		if target, err := strconv.ParseUint(operands[0], 0, 64); err == nil {
			return step{flow: flowBranch, target: target}
		}
		if strings.HasSuffix(operands[0], "(SB)") {
			return step{} // conditional tail call; otherwise falls through
		}
		return step{problem: fmt.Sprintf("indirect conditional branch %q", text)}
	}
	return step{}
}

// writesStackPointer reports whether an instruction writes SP other than by
// pushing, popping, calling or returning. In Go syntax the destination is the
// last operand.
func writesStackPointer(mnemonic string, operands []string) bool {
	if strings.HasPrefix(mnemonic, "LEAVE") || strings.HasPrefix(mnemonic, "ENTER") {
		return true
	}
	usesSP := false
	for _, operand := range operands {
		usesSP = usesSP || operand == "SP"
	}
	if !usesSP {
		return false
	}
	for _, exchange := range []string{"XCHG", "XADD", "CMPXCHG"} {
		if strings.HasPrefix(mnemonic, exchange) {
			return true
		}
	}
	if operands[len(operands)-1] != "SP" {
		return false
	}
	switch {
	case strings.HasPrefix(mnemonic, "PUSH"), strings.HasPrefix(mnemonic, "CMP"), strings.HasPrefix(mnemonic, "TEST"),
		mnemonic == "BTQ", mnemonic == "BTL", mnemonic == "BTW":
		return false
	}
	return true
}

// adjustment accepts the four ways compiled code moves SP by itself:
// SUBQ $n, SP and ADDQ $n, SP by a constant (a negative constant moves the
// other way), LEAQ n(SP), SP, which LLVM uses to release a frame before a tail
// call without touching the flags, and ANDQ $-n, SP to realign it. Anything
// else - a register, a load, LEAQ from another base or with an index, LEAVE -
// makes the depth depend on run-time values.
func adjustment(text, mnemonic string, operands []string) step {
	dynamic := step{problem: fmt.Sprintf("%q changes SP by an amount only known at run time", text)}
	if mnemonic == "LEAQ" && len(operands) == 2 {
		if match := stackPointerOffset.FindStringSubmatch(operands[0]); match != nil {
			value, err := displacement(match[1])
			if err == nil {
				return step{delta: -value}
			}
		}
		return dynamic
	}
	if len(operands) != 2 || !strings.HasPrefix(operands[0], "$") {
		return dynamic
	}
	value, err := strconv.ParseInt(operands[0][1:], 0, 64)
	if err != nil {
		return dynamic
	}
	switch mnemonic {
	case "SUBQ":
		return step{delta: value}
	case "ADDQ":
		return step{delta: -value}
	case "ANDQ":
		alignment := -value
		if alignment < 8 || alignment > maxAlignment || alignment&(alignment-1) != 0 {
			return step{problem: fmt.Sprintf("%q realigns SP by more than %d bytes", text, maxAlignment)}
		}
		return step{delta: alignment - 1}
	}
	return dynamic
}

// scrubInstructions is goos_gc_scrub in userspace/runtime/amd64.S as go tool
// objdump prints it, branch target aside.
var scrubInstructions = []string{
	"LEAQ 0xfffff000(SP), AX",
	"CMPQ AX, DI",
	"CMOVB DI, AX",
	"MOVQ SP, CX",
	"SUBQ AX, CX",
	"JBE",
	"SHRQ $0x3, CX",
	"MOVQ AX, DI",
	"XORL AX, AX",
	"CLD",
	"REP; STOSQ AX, ES:0(DI)",
	"RET",
}

// swapInstructions is goos_g_swap, the goroutine runtime's stack switch
// (ADR 0204): six pushes on the stack it leaves, then the stack it loads,
// which another goroutine or scheduler stack prepared.
var swapInstructions = []string{
	"PUSHQ R15", "PUSHQ R14", "PUSHQ R13", "PUSHQ R12", "PUSHQ BP", "PUSHQ BX",
	"MOVQ SP, 0(SI)", "MOVQ DI, SP",
	"POPQ BX", "POPQ BP", "POPQ R12", "POPQ R13", "POPQ R14", "POPQ R15",
	"RET",
}

// swapFrame is what goos_g_swap pushes before it leaves the stack.
const swapFrame = 6 * 8

// preemptInstructions is goos_preempt_entry (ADR 0204). The kernel enters
// it 128 bytes below the interrupted stack pointer; it pushes the return
// slot, the flags, and fifteen registers, keeps that depth in BX, and below
// it an aligned 512-byte FXSAVE area, filled before the kernel call, and the
// call into the runtime.
var preemptInstructions = []string{
	"PUSHL $0x0", "PUSHFQ",
	"PUSHQ AX", "PUSHQ BX", "PUSHQ CX", "PUSHQ DX", "PUSHQ SI", "PUSHQ DI", "PUSHQ BP",
	"PUSHQ R8", "PUSHQ R9", "PUSHQ R10", "PUSHQ R11", "PUSHQ R12", "PUSHQ R13", "PUSHQ R14", "PUSHQ R15",
	"MOVQ SP, BX", "SUBQ $0x200, SP", "ANDQ $-0x10, SP", "FXSAVE64 0(SP)",
	"LEAQ 0x80(BX), DI", "MOVL $0x44, AX", "INT $0x80", "TESTQ AX, AX", "JNE", "CLD", "CALL",
	"FXRSTOR64 0(SP)", "MOVQ BX, SP",
	"POPQ R15", "POPQ R14", "POPQ R13", "POPQ R12", "POPQ R11", "POPQ R10", "POPQ R9", "POPQ R8",
	"POPQ BP", "POPQ DI", "POPQ SI", "POPQ DX", "POPQ CX", "POPQ BX", "POPQ AX", "POPFQ",
	"RET $0x80", "UD2",
}

// preemptFrame is the red zone, seventeen pushed words, the FXSAVE area and
// its alignment, and the return address of the call.
const preemptFrame = 128 + 17*8 + 512 + 15 + 8

// preemptCallPinned also checks the operand of the sequence's only call.
// Legacy scheduler:none images resolve the unused weak hook to zero;
// goroutine images must call the reviewed preemption runtime entry.
func preemptCallPinned(instructions []instruction) bool {
	if len(instructions) == 0 || instructions[len(instructions)-1].text != "UD2" {
		return false
	}
	var call, branch bool
	for _, ins := range instructions {
		if strings.HasPrefix(ins.text, "CALL ") {
			call = ins.text == "CALL goos_g_preempt(SB)" || ins.text == "CALL 0x0"
		}
		if strings.HasPrefix(ins.text, "JNE ") {
			target, err := strconv.ParseUint(strings.TrimPrefix(ins.text, "JNE "), 0, 64)
			branch = err == nil && target == instructions[len(instructions)-1].address
		}
	}
	return call && branch
}

// sameInstructions reports whether a function is exactly want; a want entry
// that is a bare mnemonic matches that mnemonic with any operand.
func sameInstructions(instructions []instruction, want []string) bool {
	if len(instructions) != len(want) {
		return false
	}
	for index, instruction := range instructions {
		mnemonic, _, _ := strings.Cut(instruction.text, " ")
		if instruction.text != want[index] && !(mnemonic == want[index] && !strings.Contains(want[index], " ")) {
			return false
		}
	}
	return true
}

// analysis is what one function does to the stack: frame is the deepest SP
// gets below its value at entry, reach the deepest address the function
// writes or reads below it.
type analysis struct {
	frame, reach int64
	problems     []string
}

// analyzeFunction checks fn as code of an image with goroutine preemption,
// the stricter case.
func analyzeFunction(fn function) (int64, []string) {
	result := analyze(fn, true)
	return result.frame, result.problems
}

// analyze follows control flow so that code after an early return is still
// counted at the depth it runs at. Where two paths meet at different depths -
// after a call that does not return, the next block is reached by a jump
// only - the deeper one is kept, which can only overstate a frame. Jump-table
// targets are analysed at the depth of the indirect jump.
//
// Two properties bound the stack (ADR 0088). Guard reach: no access may lie
// so far below the entry SP that it skips the guard. The preemption red
// zone: in an image with goroutine preemption the kernel diverts a thread to
// RSP-128, so data more than 128 bytes below SP can be overwritten at any
// instruction. Without preemption nothing uses memory below SP, so a leaf
// function may address further down while its reach stays inside the guard.
func analyze(fn function, preemptible bool) analysis {
	// TinyGo's continuation restores an ancestor frame's saved SP without
	// allocating or accessing memory through the restored stack pointer.
	if fn.name == "tinygo_longjmp" && len(fn.instructions) == 3 &&
		fn.instructions[0].text == "MOVQ 0(DI), SP" &&
		fn.instructions[1].text == "MOVQ 0x8(DI), AX" &&
		fn.instructions[2].text == "JMP AX" {
		return analysis{}
	}
	// The collector's stack scrub zeroes from max(SP - 4096, DI) up to SP
	// through DI, where DI is the stack base the runtime passes: at most
	// 4096 bytes below the return address, inside the guard if the base
	// were wrong, and none below the stack (ADR 0202). Any other
	// instruction sequence is analysed, and rejected, like any function.
	if fn.name == "goos_gc_scrub" && sameInstructions(fn.instructions, scrubInstructions) {
		return analysis{reach: 4096}
	}
	// The goroutine runtime's stack switch and preemption entry move SP to
	// a stack the runtime made or restore the one they saved; the depth
	// each reaches on the stack it starts on is fixed (ADR 0204).
	if fn.name == "goos_g_swap" && sameInstructions(fn.instructions, swapInstructions) {
		return analysis{frame: swapFrame, reach: swapFrame}
	}
	if fn.name == "goos_preempt_entry" && sameInstructions(fn.instructions, preemptInstructions) && preemptCallPinned(fn.instructions) {
		return analysis{frame: preemptFrame, reach: preemptFrame}
	}
	frame, depth, steps, problems := depths(fn)
	if len(problems) != 0 {
		return analysis{frame: frame, reach: frame, problems: problems}
	}
	return accessReach(fn, steps, depth, frame, preemptible)
}

// depths returns the deepest depth in fn and the depth before each
// instruction.
func depths(fn function) (int64, []int64, []step, []string) {
	count := len(fn.instructions)
	steps := make([]step, count)
	index := make(map[uint64]int, count)
	var problems []string
	for position, instruction := range fn.instructions {
		steps[position] = decode(instruction.text)
		index[instruction.address] = position
		if steps[position].problem != "" {
			problems = append(problems, fmt.Sprintf("%s at %#x: %s", fn.name, instruction.address, steps[position].problem))
		}
	}
	if len(problems) != 0 || count == 0 {
		return 0, nil, steps, problems
	}
	depth := make([]int64, count)
	known := make([]bool, count)
	var work []int
	visits := 0
	visit := func(position int, value int64) {
		if position >= count || known[position] && depth[position] >= value {
			return
		}
		known[position], depth[position] = true, value
		work = append(work, position)
	}
	visitTarget := func(current step, value int64) {
		if position, found := index[current.target]; found {
			visit(position, value)
		}
	}
	largest, indirect, reachable := int64(0), int64(-1), int64(-1)
	visit(0, 0)
	for {
		for len(work) != 0 {
			position := work[len(work)-1]
			work = work[:len(work)-1]
			current := steps[position]
			after := depth[position] + current.delta
			largest = max(largest, after)
			if visits++; after > divergenceLimit || visits > 64*count {
				return largest, depth, steps, []string{fmt.Sprintf("%s: stack depth does not converge (allocation in a loop?)", fn.name)}
			}
			switch current.flow {
			case flowNext:
				visit(position+1, after)
			case flowJump:
				visitTarget(current, after)
			case flowBranch:
				visit(position+1, after)
				visitTarget(current, after)
			case flowIndirect:
				indirect = max(indirect, after)
			}
		}
		// Jump-table targets run at the depth of the jump into them. Code
		// without a known way in is assumed to run at the deepest point
		// reachable from the entry. Neither estimate feeds on itself, so
		// unreached blocks cannot inflate one another.
		if reachable < 0 {
			reachable = largest
		}
		seed := reachable
		if indirect >= 0 {
			seed = indirect
		}
		unreached := -1
		for position := range known {
			if !known[position] {
				unreached = position
				break
			}
		}
		if unreached < 0 {
			return largest, depth, steps, nil
		}
		visit(unreached, seed)
	}
}

func checkFunctions(functions []function, limit int64) report {
	result := report{limit: limit, largest: -1}
	preemptible := preemptionActive(functions)
	for _, fn := range functions {
		analyzed := analyze(fn, preemptible)
		frame := analyzed.frame
		result.problems = append(result.problems, analyzed.problems...)
		if analyzed.reach > limit+redZoneSize && frame <= limit {
			result.problems = append(result.problems, fmt.Sprintf(
				"%s: reaches %d bytes below its entry SP, past %d bytes (a %d-byte guard less a return address)",
				fn.name, analyzed.reach, limit+redZoneSize, loader.StackGuardSize))
		}
		result.frames = append(result.frames, namedFrame{name: fn.name, size: frame})
		if frame > result.largest {
			result.largest, result.function = frame, fn.name
		}
		if frame > limit {
			result.problems = append(result.problems, fmt.Sprintf(
				"%s: frame of %d bytes exceeds %d bytes (a %d-byte guard less a %d-byte margin); move its large locals to the heap or a global",
				fn.name, frame, limit, loader.StackGuardSize, userMargin))
		}
	}
	return result
}
