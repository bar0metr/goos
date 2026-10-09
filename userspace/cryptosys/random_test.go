//go:build !goos_userspace

package cryptosys

import (
	"goos/kernel/kcall"
	"testing"
)

func TestReaderEmpty(t *testing.T) {
	if n, err := (Reader{}).Read(nil); n != 0 || err != nil {
		t.Fatalf("empty Read = %d, %v", n, err)
	}
}

func TestReaderKernelFailure(t *testing.T) {
	p := []byte{1, 2, 3}
	n, err := (Reader{}).Read(p)
	failure, ok := err.(randomError)
	if n != 0 || !ok || failure.status != kcall.StatusUnsupported {
		t.Fatalf("Read = %d, %v", n, err)
	}
	for _, value := range p {
		if value != 0 {
			t.Fatal("failure retained buffer bytes")
		}
	}
}
