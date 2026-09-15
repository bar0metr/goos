# ADR 0015: Restartable IPC waits

## Status

Accepted

## Decision

Blocking receive uses a dedicated `IPCWaitReceive` dispatch entry instead of
expanding the general native-call handler interface. Its fixed descriptor holds
the receive buffer, maximum wire capacity, and relative timeout ticks.

When no message is available, the kernel registers the task before returning a
`Block` action. The assembly bridge preserves the original call arguments,
normalizes the interrupt frame to the scheduler layout, rewinds the user RIP to
the `int 0x80` instruction, removes the task from the runnable mask, and selects
its successor. Wakeup therefore retries validation and dequeue atomically
instead of resuming with stale kernel state.

The existing kernel timer task expires the bounded wait set using the monotonic
PIT tick count. It records `Timeout` before making the task runnable. Successful
sends wake the waiter after the message is committed to the channel queue.

All assembly-to-Go calls align RSP to the Win64 16-byte boundary. Generated Go
prologues may use aligned SIMD moves, so relying on the incidental stack-frame
size of a previous build is invalid.

## Consequences

No Go function is called reentrantly from IRQ0. Wait memory remains statically
bounded. Receive revalidates the user descriptor and channel capability after
every wake. The current task identity is packed into unused low bits of the
page-aligned root only for the dedicated wait entry and is masked before use.
