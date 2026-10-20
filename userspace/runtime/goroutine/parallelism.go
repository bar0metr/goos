package goroutine

import "goos/userspace/runtime/tls"

// parallelism is bounded by online CPUs, the process task quota, and TCB
// slots. Failure to read either bound leaves one execution permit.
func parallelism(online, taskLimit int32) int32 {
	return max(int32(1), min(online, taskLimit, int32(tls.Slots)))
}
