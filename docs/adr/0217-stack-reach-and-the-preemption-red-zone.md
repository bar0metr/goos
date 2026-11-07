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
and calls nothing. The probe images have no `goos_preempt_entry`. The
checker read the displacement and ignored the index. AX starts at 16 and
counts to 63 in that loop, so the two loads reach SP-76 and SP-80: inside
the red zone.

The checker also mixed two properties. The 64 KiB guard catches an
overflow only if no access jumps past it. The 128-byte red zone matters
only when something else writes below SP: the kernel diverts a preempted
goroutine thread to `RSP - 128`, and `goos_preempt_entry` pushes from
there. A scheduler-none image registers no such entry, and kernel task
switches save user state in kernel memory.

## Decision

`tools/stackframes` computes each function's reach: the deepest of its SP
depth, the return slot each `CALL` writes, and its lowest SP-relative
operand, in bytes below the entry SP. Reach past the guard less a return
address fails the build, whatever the frame.

An operand `disp(SP)(R*s)` lowest address is `disp + s * start` when the
tool proves R's range. It proves one loop shape, the one `sha256.block`
uses:

```
MOVL $start, R      ; encoding B8+r imm32, checked against the text
NOP...
header: body        ; no branch, call, or write to any part of R
INCQ R
CMPQ R, $limit      ; start < limit <= 2^31
JNE header
```

No other jump may enter the loop or its padding, and a function with a
jump table proves nothing. Without a proof, a displacement below -128
fails the build; above it the index counts as zero, as before this ADR.
An SP operand in any other form fails the build.

An access below SP-128 passes only when the image has no goroutine
preemption and the function contains no `CALL`, since a callee writes
below the caller's SP. It then counts in the function's reach. The tool
decides preemption from the ELF: active when `goos_preempt_entry` calls
`goos_g_preempt`. A tail `JMP` is not a call. The pinned runtime sequences
stay exact.

## Consequences

`sha256.block` passes under the preemption rule as well; its reach is its
232-byte frame. `tools/stackframes/testdata/sha256-block.objdump` holds the
function from the probe ELF, and its tests check the decoded displacements,
the frame, the missing call and preemption entry, and that starting the
counter at zero needs the scheduler-none leaf rule. `make
test-crypto-frames` builds the real `sha256` and `ecdsa` probes with the
compatibility root and runs the checker on their final ELFs.

The tool still trusts an unproven index above the red zone. Loops of
another shape, a decrementing counter or a counter set in a register,
prove nothing; code that needs them below SP-128 must change or teach the
tool a shape with tests.
