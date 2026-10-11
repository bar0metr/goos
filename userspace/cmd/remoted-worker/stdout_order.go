package main

import "goos/kernel/kcall"

// drainStdoutBatch leaves a terminal request for the next loop when a full
// batch was read. SSH input and protocol events run between those batches.
func drainStdoutBatch(read func([]byte) (int, kcall.Status), consume func([]byte), free func() int, scratch []byte, limit int) (pending, activity, closed, ok bool) {
	for count := 0; count < limit; count++ {
		if free() < 1024 {
			return true, activity, false, true
		}
		n, status := read(scratch)
		if n > 0 {
			consume(scratch[:n])
			activity = true
		}
		if status == kcall.StatusPeerDied {
			return false, activity, true, true
		}
		if status != kcall.StatusOK && status != kcall.StatusResourceExhausted {
			return false, activity, false, false
		}
		if n == 0 {
			return false, activity, false, true
		}
	}
	return true, activity, false, true
}
