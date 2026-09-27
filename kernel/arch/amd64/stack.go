package amd64

const (
	// KernelStackGuardSize is the unmapped guard below every kernel stack the
	// scheduler, the TSS and user tasks' kernel entry use (ADR 0006). TinyGo's
	// UEFI runtime makes the stack probe LLVM emits for large frames a no-op,
	// so tools/stackframes bounds every kernel frame by this size at build
	// time instead. Two pages leave room for kcall.Dispatch, into which the
	// native-call handlers are inlined (ADR 0088).
	KernelStackGuardSize = 2 * 4096
	// kernelStackPages is the usable size of every kernel stack. The
	// allocation is these pages plus the guard. Scheduler metadata records the
	// physical allocation separately from its higher-half virtual address.
	kernelStackPages = uint64(4)
	// BootStackSize is the stack the boot task runs on once paging is up: the
	// rest of boot, the validation self-tests, and the service loop. It
	// replaces the goroutine stack TinyGo's runtime allocates from the heap,
	// which has no guard (ADR 0091).
	BootStackSize = 64 << 10
	// BootStackGuardSize is the unmapped gap below the boot stack. It is
	// wider than the other kernel stacks' guard because the boot task's own
	// functions, the self-tests above all, have frames larger than that.
	BootStackGuardSize = 32 << 10
	// faultStackPages is the stack a double fault is delivered on, through the
	// TSS's first interrupt stack, so an overflow of any other kernel stack is
	// reported instead of resetting the machine.
	faultStackPages = uint64(1)
)
