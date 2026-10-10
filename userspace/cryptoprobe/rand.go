//go:build cryptoprobe_rand

package cryptoprobe

import (
	"crypto/rand"
	"errors"
	"goos/userspace/cryptosys"
)

const Name = "rand"

func Run() error {
	reader := cryptosys.Reader{}
	var available [1]byte
	// Go 1.25 rand.Read aborts on reader errors; check kernel availability first.
	if _, err := reader.Read(available[:]); err != nil {
		return err
	}
	previous := rand.Reader
	rand.Reader = reader
	defer func() { rand.Reader = previous }()
	var sample [32]byte
	n, err := rand.Read(sample[:])
	if err != nil {
		return err
	}
	if n != len(sample) {
		return errors.New("rand.Read returned a short sample")
	}
	return nil
}
