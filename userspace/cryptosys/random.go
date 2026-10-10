// Package cryptosys adapts kernel entropy to Go crypto readers.
package cryptosys

import (
	"goos/kernel/kcall"
	"goos/userspace/runtime/sys"
)

// Reader obtains entropy only from the kernel generator.
type Reader struct{}

type randomError struct{ status kcall.Status }

func (randomError) Error() string { return "kernel random source unavailable" }

func (Reader) Read(p []byte) (int, error) {
	if result := sys.RandomRead(p); result != kcall.StatusOK {
		return 0, randomError{status: result}
	}
	return len(p), nil
}
