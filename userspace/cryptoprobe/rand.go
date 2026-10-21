//go:build cryptoprobe_rand

package cryptoprobe

import (
	"io"

	"goos/userspace/cryptosys"
)

const Name = "rand"

func Run() error { return probeRandom(cryptosys.Reader{}) }

// probeRandom checks the kernel-backed reader without replacing the entropy
// source of other crypto callers in the process.
func probeRandom(reader io.Reader) error {
	var sample [32]byte
	_, err := io.ReadFull(reader, sample[:])
	return err
}
