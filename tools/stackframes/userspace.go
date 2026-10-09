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
	// Realigning lowers the stack pointer by at most n-8 bytes, which the
	// depth counts.
	maxAlignment = 64
	// divergenceLimit stops the analysis of a function whose depth keeps
	// growing, which only a loop that allocates on the stack can cause.
	divergenceLimit = int64(1) << 24
)

type instruction struct {
	address uint64
	text    string
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
			last.instructions = append(last.instructions, instruction{address: address, text: fields[3]})
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

// step is what one instruction does to the stack depth and to control flow.
type step struct {
	delta   int64
	flow    flow
	target  uint64
	problem string
}

var belowStackPointer = regexp.MustCompile(`(-0x[0-9a-f]+)\(SP\)`)

// stackPointerOffset matches a memory operand that is SP plus a constant and
// nothing else.
var stackPointerOffset = regexp.MustCompile(`^(-?0x[0-9a-f]+|-?[0-9]+)\(SP\)$`)

func decode(text string) step {
	mnemonic, rest, _ := strings.Cut(text, " ")
	var operands []string
	if rest = strings.TrimSpace(rest); rest != "" {
		operands = strings.Split(rest, ", ")
	}
	if mnemonic == "?" {
		return step{problem: "undecodable instruction"}
	}
	for _, operand := range operands {
		if match := belowStackPointer.FindStringSubmatch(operand); match != nil {
			if offset, err := strconv.ParseInt(match[1], 0, 64); err != nil || offset < -redZoneSize {
				return step{problem: fmt.Sprintf("%q reaches below the %d-byte red zone", text, redZoneSize)}
			}
		}
	}
	if writesStackPointer(mnemonic, operands) {
		return adjustment(text, mnemonic, operands)
	}
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
			value, err := strconv.ParseInt(match[1], 0, 64)
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
		return step{delta: alignment - 8}
	}
	return dynamic
}

// analyzeFunction returns the deepest the stack pointer gets below its value
// at the function's entry, following control flow so that code after an
// early return is still counted at the depth it runs at. Where two paths meet
// at different depths - after a call that does not return, the next block is
// reached by a jump only - the deeper one is kept, which can only overstate a
// frame. Jump-table targets are analysed at the depth of the indirect jump.
func analyzeFunction(fn function) (int64, []string) {
	// TinyGo's continuation restores an ancestor frame's saved SP without
	// allocating or accessing memory through the restored stack pointer.
	if fn.name == "tinygo_longjmp" && len(fn.instructions) == 3 &&
		fn.instructions[0].text == "MOVQ 0(DI), SP" &&
		fn.instructions[1].text == "MOVQ 0x8(DI), AX" &&
		fn.instructions[2].text == "JMP AX" {
		return 0, nil
	}
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
		return 0, problems
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
				return largest, []string{fmt.Sprintf("%s: stack depth does not converge (allocation in a loop?)", fn.name)}
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
			return largest, nil
		}
		visit(unreached, seed)
	}
}

func checkFunctions(functions []function, limit int64) report {
	result := report{limit: limit, largest: -1}
	for _, fn := range functions {
		frame, problems := analyzeFunction(fn)
		result.problems = append(result.problems, problems...)
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
