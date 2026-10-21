package goroutine

import (
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/userspace/runtime/sys"
)

// irqWaitOps keeps the IRQ adapter's orchestration identical in native
// builds and host tests.
type irqWaitOps struct {
	// probe validates IRQWait's line and consumes an already pending event.
	probe func(handle.ID) kcall.Status
	// take consumes an event after readiness: IRQTake.
	take func(handle.ID) kcall.Status
	wait func(*[kcall.WaitAnyMaxObjects]sys.Interest, int, int64) (uint64, kcall.Status)
}

// waitForIRQ first probes through IRQWait's nonblocking mode, preserving
// its capability and line checks before waiting or taking through the wider
// WaitAny/IRQTake interface. A second taker may consume a ready event before
// this G resumes, so NotFound repeats the readiness wait.
func waitForIRQ(irq handle.ID, ops irqWaitOps) kcall.Status {
	if status := ops.probe(irq); status != kcall.StatusNotFound {
		return status
	}
	var interests [kcall.WaitAnyMaxObjects]sys.Interest
	interests[0] = sys.Interest{Handle: irq, Events: kcall.WaitReadable}
	for {
		if _, status := ops.wait(&interests, 1, 0); status != kcall.StatusOK {
			return status
		}
		if status := ops.take(irq); status != kcall.StatusNotFound {
			return status
		}
	}
}
