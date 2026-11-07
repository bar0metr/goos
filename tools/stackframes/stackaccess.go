package main

import (
	"fmt"
	"regexp"

	"golang.org/x/arch/x86/x86asm"
)

// preemptionActive reports whether the image registers the goroutine
// runtime's preemption: goos_preempt_entry exists and calls
// goos_g_preempt. A scheduler-none image has no entry, or one whose weak
// call resolved to address zero.
func preemptionActive(functions []function) bool {
	for _, fn := range functions {
		if fn.name != "goos_preempt_entry" {
			continue
		}
		for _, ins := range fn.instructions {
			if ins.text == "CALL goos_g_preempt(SB)" {
				return true
			}
		}
	}
	return false
}

// accessReach checks every SP-relative operand of fn against the red zone
// and returns the function's reach: the deepest of its SP depth, the return
// slot each call writes, and every stack operand's lowest address.
func accessReach(fn function, steps []step, depth []int64, frame int64, preemptible bool) analysis {
	result := analysis{frame: frame, reach: frame}
	calls := false
	for _, current := range steps {
		calls = calls || current.call
	}
	bounds := countedLoops(fn, steps)
	for position, current := range steps {
		ins := fn.instructions[position]
		if current.call {
			result.reach = max(result.reach, depth[position]+returnAddressSize)
		}
		for _, access := range current.accesses {
			lowest := access.disp
			if access.index != "" {
				// The compiler answers for an index's bounds, as for any
				// computed pointer. A base below the red zone is the
				// tool's to clear, so it needs the index proved.
				start, proven := bounds[position][registerFamily(access.index)]
				switch {
				case proven:
					lowest = access.disp + access.scale*start
				case access.disp < -redZoneSize:
					result.problems = append(result.problems, fmt.Sprintf(
						"%s at %#x: %q indexes the stack below the %d-byte red zone with a register the tool cannot bound",
						fn.name, ins.address, ins.text, redZoneSize))
					continue
				}
			}
			if lowest >= -redZoneSize {
				result.reach = max(result.reach, depth[position]-lowest)
				continue
			}
			switch {
			case preemptible:
				result.problems = append(result.problems, fmt.Sprintf(
					"%s at %#x: %q reaches %d bytes below SP, past the %d-byte red zone that goroutine preemption keeps",
					fn.name, ins.address, ins.text, -lowest, redZoneSize))
			case calls:
				result.problems = append(result.problems, fmt.Sprintf(
					"%s at %#x: %q reaches %d bytes below SP in a function that calls, and a callee would overwrite it",
					fn.name, ins.address, ins.text, -lowest))
			default:
				result.reach = max(result.reach, depth[position]-lowest)
			}
		}
	}
	return result
}

// registerFamily names the 64-bit register a register operand is part of.
func registerFamily(name string) string {
	switch name {
	case "AL", "AH":
		return "AX"
	case "BL", "BH":
		return "BX"
	case "CL", "CH":
		return "CX"
	case "DL", "DH":
		return "DX"
	case "SIB":
		return "SI"
	case "DIB":
		return "DI"
	case "BPB":
		return "BP"
	case "SPB":
		return "SP"
	}
	if match := numberedRegister.FindStringSubmatch(name); match != nil {
		return match[1]
	}
	return name
}

var numberedRegister = regexp.MustCompile(`^(R(?:8|9|1[0-5]))[BWL]?$`)

// countedLoops proves bounds on loop counters of this shape:
//
//	init                    ; MOVL $start, R or XORL R, R, both of which
//	...                     ;   clear R's upper half; then code that
//	header: ...             ;   leaves R alone and does not branch
//	step                    ; INCQ R or ADDQ $k, R: the body's only write to R
//	...
//	CMPQ R, $limit
//	JNE header
//
// with start < limit <= 2^31, (limit-start) a multiple of k, no branch in
// the loop and no way in but the fall-through from init and the JNE. R then
// holds start, start+k, ... limit-k before the step, so the map gives start
// for each loop position. The machine code decides every write; anything
// else proves nothing.
func countedLoops(fn function, steps []step) map[int]map[string]int64 {
	bounds := make(map[int]map[string]int64)
	count := len(fn.instructions)
	index := make(map[uint64]int, count)
	decoded := make([]x86asm.Inst, count)
	valid := make([]bool, count)
	for position, ins := range fn.instructions {
		index[ins.address] = position
		decoded[position], valid[position] = decodeMachine(ins.encoding)
	}
	sources := make(map[int][]int)
	for position, current := range steps {
		switch current.flow {
		case flowIndirect:
			return bounds // a jump table could enter any loop
		case flowJump, flowBranch:
			if target, found := index[current.target]; found {
				sources[target] = append(sources[target], position)
			}
		}
	}
	for latch := 2; latch < count; latch++ {
		if !valid[latch] || !valid[latch-1] || decoded[latch].Op != x86asm.JNE || steps[latch].flow != flowBranch {
			continue
		}
		header, found := index[steps[latch].target]
		cmp := decoded[latch-1]
		reg, isReg := cmp.Args[0].(x86asm.Reg)
		limit, isImm := cmp.Args[1].(x86asm.Imm)
		number, width, isGPR := gpr(reg)
		if !found || header >= latch-1 || cmp.Op != x86asm.CMP || !isReg || !isImm || !isGPR || width != 64 || limit <= 0 || limit > 1<<31 {
			continue
		}
		if len(sources[header]) != 1 || sources[header][0] != latch {
			continue
		}
		stride, writer, ok := int64(0), -1, true
		for position := header; position <= latch-1 && ok; position++ {
			if position > header && len(sources[position]) != 0 || !valid[position] || steps[position].flow != flowNext {
				ok = false
				break
			}
			if !writesRegister(decoded[position], number) {
				continue
			}
			inst := decoded[position]
			ok = writer < 0 && inst.Args[0] == reg
			writer = position
			switch {
			case ok && inst.Op == x86asm.INC:
				stride = 1
			case ok && inst.Op == x86asm.ADD:
				k, isK := inst.Args[1].(x86asm.Imm)
				ok, stride = isK && k > 0, int64(k)
			default:
				ok = false
			}
		}
		if !ok || writer < 0 || len(sources[latch]) != 0 {
			continue
		}
		start, ok := loopStart(decoded, valid, steps, sources, header, number)
		if !ok || start >= int64(limit) || (int64(limit)-start)%stride != 0 {
			continue
		}
		name := []string{"AX", "CX", "DX", "BX", "SP", "BP", "SI", "DI",
			"R8", "R9", "R10", "R11", "R12", "R13", "R14", "R15"}[number]
		for position := header; position < latch; position++ {
			if bounds[position] == nil {
				bounds[position] = make(map[string]int64)
			}
			bounds[position][name] = start
		}
	}
	return bounds
}

// loopStart walks back from header to the instruction that sets the
// counter, through code that neither writes it, branches, nor is a branch
// target.
func loopStart(decoded []x86asm.Inst, valid []bool, steps []step, sources map[int][]int, header, number int) (int64, bool) {
	for position := header - 1; position >= 0 && position >= header-64; position-- {
		if !valid[position] || steps[position].flow != flowNext {
			return 0, false
		}
		inst := decoded[position]
		if writesRegister(inst, number) {
			reg, _ := inst.Args[0].(x86asm.Reg)
			dest, width, _ := gpr(reg)
			if dest != number || width != 32 {
				return 0, false
			}
			switch inst.Op {
			case x86asm.MOV:
				value, isImm := inst.Args[1].(x86asm.Imm)
				return int64(uint32(value)), isImm
			case x86asm.XOR:
				return 0, inst.Args[1] == inst.Args[0]
			}
			return 0, false
		}
		if len(sources[position]) != 0 {
			return 0, false
		}
	}
	return 0, false
}
