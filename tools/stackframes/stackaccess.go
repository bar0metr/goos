package main

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
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
// slot each call writes, and its lowest stack operand.
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
				start, proven := bounds[position][registerFamily(access.index)]
				switch {
				case proven:
					lowest = access.disp + access.scale*start
				case access.disp < -redZoneSize:
					// A negative index could reach anywhere below.
					result.problems = append(result.problems, fmt.Sprintf(
						"%s at %#x: %q indexes the stack below the %d-byte red zone with a register the tool cannot bound",
						fn.name, ins.address, ins.text, redZoneSize))
					continue
				}
				// An index the tool cannot bound counts as zero, as it
				// always has above the red zone.
			}
			if lowest >= -redZoneSize {
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

// loopSafe lists instructions that write nothing but their last operand
// and do not branch. A counted loop's body may hold only these.
var loopSafe = map[string]bool{
	"MOVB": true, "MOVW": true, "MOVL": true, "MOVQ": true, "MOVZX": true, "MOVSX": true,
	"MOVAPS": true, "MOVUPS": true, "MOVDQA": true, "MOVDQU": true,
	"ADDL": true, "ADDQ": true, "SUBL": true, "SUBQ": true, "ANDL": true, "ANDQ": true,
	"ORL": true, "ORQ": true, "XORL": true, "XORQ": true, "XORPS": true, "PXOR": true,
	"ROLL": true, "ROLQ": true, "RORL": true, "RORQ": true, "SHLL": true, "SHLQ": true,
	"SHRL": true, "SHRQ": true, "SARL": true, "SARQ": true, "NOTL": true, "NOTQ": true,
	"NEGL": true, "NEGQ": true, "BSWAP": true, "LEAL": true, "LEAQ": true,
	"CMPB": true, "CMPW": true, "CMPL": true, "CMPQ": true,
	"TESTB": true, "TESTW": true, "TESTL": true, "TESTQ": true,
}

// countedLoops proves bounds on loop counters. It accepts one shape:
//
//	MOVL $start, R          ; B8+r imm32, which clears R's upper half
//	NOP...                  ; optional padding
//	header: ...body...      ; writes neither R nor control flow
//	INCQ R
//	CMPQ R, $limit
//	JNE header
//
// with start < limit <= 2^31, entered only through the MOVL and left only
// through the JNE's fall-through. R then holds start..limit-1 throughout
// the body, so the map gives start for each body position. Anything else
// proves nothing.
func countedLoops(fn function, steps []step) map[int]map[string]int64 {
	bounds := make(map[int]map[string]int64)
	count := len(fn.instructions)
	index := make(map[uint64]int, count)
	for position, ins := range fn.instructions {
		index[ins.address] = position
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
		inc, cmp, jne := fn.instructions[latch-2].text, fn.instructions[latch-1].text, fn.instructions[latch].text
		register, ok := strings.CutPrefix(inc, "INCQ ")
		if !ok || registerFamily(register) != register {
			continue
		}
		limitText, ok := strings.CutPrefix(cmp, "CMPQ "+register+", $")
		if !ok {
			continue
		}
		limit, err := strconv.ParseInt(limitText, 0, 64)
		if err != nil || limit <= 0 || limit > 1<<31 {
			continue
		}
		targetText, ok := strings.CutPrefix(jne, "JNE ")
		if !ok {
			continue
		}
		target, err := strconv.ParseUint(targetText, 0, 64)
		header, found := index[target]
		if err != nil || !found || header >= latch-2 {
			continue
		}
		start, ok := loopEntry(fn, sources, header, latch, register)
		if !ok || start >= limit || !loopBody(fn, sources, header, latch-2, register) {
			continue
		}
		for position := header; position <= latch-2; position++ {
			if bounds[position] == nil {
				bounds[position] = make(map[string]int64)
			}
			bounds[position][register] = start
		}
	}
	return bounds
}

// loopEntry finds the MOVL that sets the counter before header and checks
// that the JNE at latch is the only other way into the loop.
func loopEntry(fn function, sources map[int][]int, header, latch int, register string) (int64, bool) {
	if len(sources[header]) != 1 || sources[header][0] != latch {
		return 0, false
	}
	for position := header + 1; position <= latch; position++ {
		if len(sources[position]) != 0 {
			return 0, false
		}
	}
	position := header - 1
	for position >= 0 && strings.HasPrefix(fn.instructions[position].text, "NOP") {
		if len(sources[position]) != 0 {
			return 0, false
		}
		position--
	}
	if position < 0 {
		return 0, false
	}
	ins := fn.instructions[position]
	valueText, ok := strings.CutPrefix(ins.text, "MOVL $")
	if !ok || !strings.HasSuffix(valueText, ", "+register) {
		return 0, false
	}
	value, err := strconv.ParseInt(strings.TrimSuffix(valueText, ", "+register), 0, 64)
	if err != nil || value < 0 {
		return 0, false
	}
	// Check the encoding: objdump has printed byte operations with long
	// mnemonics, and only a 32-bit move clears the upper half.
	code, err := hex.DecodeString(ins.encoding)
	if err != nil {
		return 0, false
	}
	if len(code) == 6 && code[0]&0xf8 == 0x40 { // REX without W
		code = code[1:]
	}
	if len(code) != 5 || code[0]&0xf8 != 0xb8 || int64(binary.LittleEndian.Uint32(code[1:])) != value {
		return 0, false
	}
	return value, true
}

// loopBody checks that body positions header..inc-1 neither branch nor
// write the counter.
func loopBody(fn function, sources map[int][]int, header, inc int, register string) bool {
	for position := header; position < inc; position++ {
		mnemonic, rest, _ := strings.Cut(fn.instructions[position].text, " ")
		if strings.HasPrefix(mnemonic, "NOP") {
			continue
		}
		if !loopSafe[mnemonic] {
			return false
		}
		operands := strings.Split(strings.TrimSpace(rest), ", ")
		writes := !strings.HasPrefix(mnemonic, "CMP") && !strings.HasPrefix(mnemonic, "TEST")
		if writes && registerFamily(operands[len(operands)-1]) == register {
			return false
		}
	}
	return true
}
