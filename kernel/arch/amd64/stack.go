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
	// allocation is these pages plus the guard; goos_reclaim_exited_task in
	// io.S recomputes its start from the stack top.
	kernelStackPages = uint64(4)
)
