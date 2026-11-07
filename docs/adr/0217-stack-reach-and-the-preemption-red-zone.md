# ADR 0217: Stack reach and the preemption red zone

## Status

Accepted. Refines ADR 0088 (frame bound) and ADR 0204 (goroutine
preemption).

## Context

`go tool objdump` prints a negative 32-bit displacement unsigned:
-140 reads as `0xffffff74(SP)`. Once `tools/stackframes` decoded that
form, it rejected two instructions of `crypto/internal/fips140/sha256.block`
in the `sha256` and `ecdsa` crypto probes:

| Instruction | Displacement |
| --- | ---: |
| `MOVL 0xffffff74(SP)(AX*4), SI` | -140 |
| `ADDL 0xffffff70(SP)(AX*4), CX` | -144 |

The function pushes six registers and subtracts `0xb8`, a 232-byte frame,
and calls nothing. The probe images have no `goos_preempt_entry`. AX
starts at 16 and counts to 63 in that loop, so the two loads reach SP-76
and SP-80: inside the red zone.

The checker also mixed two properties. The 64 KiB guard catches an
overflow only if no access jumps past it. The 128-byte red zone matters
only when something else writes below SP: the kernel diverts a preempted
goroutine thread to `RSP - 128`, and `goos_preempt_entry` pushes from
there. A scheduler-none image registers no such entry, and kernel task
switches save user state in kernel memory.

## Decision

`tools/stackframes` computes each function's reach: the deepest of its SP
depth, the return slot each `CALL` writes, and the lowest address of every
SP-relative operand, red zone included, in bytes below the entry SP. Reach
past the guard less a return address fails the build, whatever the frame.

The tool answers for SP and for constant displacements from it. An
operand `disp(SP)(R*s)` has its lowest address at `disp + s * start` when
the tool proves R's range. When it cannot, the address counts as `disp`:
the compiler answers for an index's bounds, as for any pointer it
computes, and `stackframes` does not verify generated array accesses. A
base below SP-128 is the exception: the red zone does not cover it, so the
build fails unless the tool proves the index. A change to SP by an amount
known only at run time always fails, and so does an SP operand in any
other form.

The tool proves one loop shape, the one `sha256.block` uses:

```
MOVL $start, R or XORL R, R   ; clear R's upper half
...                           ; code that leaves R alone and does not branch
header: ...
INCQ R or ADDQ $k, R          ; the body's only write to R
...
CMPQ R, $limit                ; start < limit <= 2^31, (limit-start) % k == 0
JNE header
```

No other jump may enter the loop, and a function with a jump table proves
nothing. The tool reads operand sizes and immediates from the machine code
with `golang.org/x/arch/x86/x86asm`, the decoder `go tool objdump` vendors,
at the same version: objdump's text has printed byte operations with 32-bit
mnemonics. An instruction the tool does not know writes every register.

An access below SP-128 passes only when the image has no goroutine
preemption and the function contains no `CALL`, since a callee writes
below the caller's SP. The tool decides preemption from the ELF: active
when `goos_preempt_entry` calls `goos_g_preempt`. A tail `JMP` is not a
call. The pinned runtime sequences stay exact.

## Consequences

`sha256.block` passes under the preemption rule as well. Its reach is 352
bytes: the 232-byte frame and `MOVL R15, -0x78(SP)`, 120 bytes below SP;
the indexed loads reach SP-80. `tools/stackframes/testdata/sha256-block.objdump`
holds the function from the probe ELF, and its tests check the decoded
displacements, the frame, the reach, the missing call and preemption
entry, and that starting the counter at zero needs the scheduler-none leaf
rule. `make test-crypto-frames` builds the real `sha256` and `ecdsa`
probes with the compatibility root and runs the checker on their final
ELFs.

Every userspace ELF of the build passes. An index that leaves its array is
a compiler or program fault the guard does not promise to catch; TinyGo's
inlined map hash, for one, indexes a stack key by a length it reads from
the map.
