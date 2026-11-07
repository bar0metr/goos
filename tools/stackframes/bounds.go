package main

import (
	"encoding/hex"
	"strings"

	"golang.org/x/arch/x86/x86asm"
)

// decodeMachine decodes one instruction's machine code; ok is false unless
// the decoder consumed exactly the bytes objdump printed. The loop proof
// reads operand sizes from here: objdump has printed byte operations with
// 32-bit mnemonics.
func decodeMachine(encoding string) (x86asm.Inst, bool) {
	code, err := hex.DecodeString(encoding)
	if err != nil || len(code) == 0 {
		return x86asm.Inst{}, false
	}
	inst, err := x86asm.Decode(code, 64)
	return inst, err == nil && inst.Len == len(code)
}

// gpr maps a register to its number, as the x86 encodes it (AX CX DX BX
// SP BP SI DI R8..R15), and its width in bits; ok is false for anything but
// a general register.
func gpr(reg x86asm.Reg) (int, int, bool) {
	switch {
	case reg >= x86asm.AL && reg <= x86asm.BL:
		return int(reg - x86asm.AL), 8, true
	case reg >= x86asm.AH && reg <= x86asm.BH:
		return int(reg - x86asm.AH), 8, true
	case reg >= x86asm.SPB && reg <= x86asm.R15B:
		return int(reg-x86asm.SPB) + 4, 8, true
	case reg >= x86asm.AX && reg <= x86asm.R15W:
		return int(reg - x86asm.AX), 16, true
	case reg >= x86asm.EAX && reg <= x86asm.R15L:
		return int(reg - x86asm.EAX), 32, true
	case reg >= x86asm.RAX && reg <= x86asm.R15:
		return int(reg - x86asm.RAX), 64, true
	}
	return 0, 0, false
}

// destinationOnly lists instructions that write no general register but
// their first operand (and the flags).
var destinationOnly = map[string]bool{
	"MOV": true, "MOVZX": true, "MOVSX": true, "MOVSXD": true, "LEA": true,
	"ADD": true, "SUB": true, "ADC": true, "SBB": true, "AND": true, "OR": true, "XOR": true,
	"NOT": true, "NEG": true, "INC": true, "DEC": true, "IMUL": true,
	"SHL": true, "SHR": true, "SAR": true, "ROL": true, "ROR": true, "RCL": true, "RCR": true,
	"SHLD": true, "SHRD": true, "BSWAP": true, "BSF": true, "BSR": true,
	"TZCNT": true, "LZCNT": true, "POPCNT": true, "BTS": true, "BTR": true, "BTC": true,
}

// noRegisterWrite lists instructions that write no general register.
var noRegisterWrite = map[string]bool{
	"CMP": true, "TEST": true, "BT": true, "NOP": true,
}

// writesRegister reports whether inst may write general register number.
// An instruction outside the two lists writes every register.
func writesRegister(inst x86asm.Inst, number int) bool {
	op := inst.Op.String()
	switch {
	case noRegisterWrite[op]:
		return false
	case op == "IMUL" && inst.Args[1] == nil:
		return true // writes AX and DX
	case destinationOnly[op], strings.HasPrefix(op, "CMOV"), strings.HasPrefix(op, "SET"):
		reg, ok := inst.Args[0].(x86asm.Reg)
		if !ok {
			return false // writes memory
		}
		dest, _, isGPR := gpr(reg)
		return isGPR && dest == number
	}
	reg, ok := inst.Args[0].(x86asm.Reg)
	vector := ok && reg >= x86asm.X0 && reg <= x86asm.X15 && !strings.HasPrefix(op, "PCMP")
	return !vector
}
