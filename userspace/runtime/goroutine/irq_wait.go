package goroutine

import (
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/userspace/runtime/sys"
)

// irqWaitOps keeps the IRQ adapter's orchestration identical in native
// builds and host tests.
type irqWaitOps struct {
	// take consumes a pending event: IRQTake.
	take func(handle.ID) kcall.Status
	wait func(*[kcall.WaitAnyMaxObjects]sys.Interest, int, int64) (uint64, kcall.Status)
}

// waitForIRQ takes the line's pending event, parking on its readiness while
// there is none. Readiness does not consume the event, and another taker may
// consume it between the wake and the take, so NotFound waits again. IRQTake
// checks the capability as IRQWait does; a line IRQWait refuses is refused
// here with the same PermissionDenied, except the network line, which
// IRQTake serves.
func waitForIRQ(irq handle.ID, ops irqWaitOps) kcall.Status {
	var interests [kcall.WaitAnyMaxObjects]sys.Interest
	interests[0] = sys.Interest{Handle: irq, Events: kcall.WaitReadable}
	for {
		status := ops.take(irq)
		if status != kcall.StatusNotFound {
			return status
		}
		if _, status = ops.wait(&interests, 1, 0); status != kcall.StatusOK {
			return status
		}
	}
}
