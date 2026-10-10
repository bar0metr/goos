//go:build cryptoprobe_ecdh

package cryptoprobe

import (
	"crypto/ecdh"
	"errors"
)

const Name = "ecdh"

func Run() error {
	// RFC 7748, Alice and Bob's X25519 agreement.
	curve := ecdh.X25519()
	alice, err := curve.NewPrivateKey(fixtureHex("77076d0a7318a57d3c16c17251b26645df4c2f87ebc0992ab177fba51db92c2a"))
	if err != nil {
		return err
	}
	bob, err := curve.NewPrivateKey(fixtureHex("5dab087e624a8a4b79e17f8b83800ee66f3bb1292618b6fd1c2f8b27ff88e0eb"))
	if err != nil {
		return err
	}
	if err := checkHex(alice.PublicKey().Bytes(), "8520f0098930a754748b7ddcb43ef75a0dbf3a0d26381af4eba4a98eaa9b4e6a"); err != nil {
		return err
	}
	if err := checkHex(bob.PublicKey().Bytes(), "de9edb7d7b7dc1b4d35b61c2ece435373f8343c85b78674dadfc7e146f882b4f"); err != nil {
		return err
	}
	shared, err := alice.ECDH(bob.PublicKey())
	if err != nil {
		return err
	}
	const want = "4a5d9d5ba4ce2de1728e3bf480350f25e07e21c947d19e3376f09b3c1e161742"
	if err := checkHex(shared, want); err != nil {
		return err
	}
	shared, err = bob.ECDH(alice.PublicKey())
	if err != nil {
		return err
	}
	if err := checkHex(shared, want); err != nil {
		return err
	}
	zero, err := curve.NewPublicKey(make([]byte, 32))
	if err != nil {
		return err
	}
	if _, err := alice.ECDH(zero); err == nil {
		return errors.New("X25519 accepted an all-zero shared secret")
	}
	return nil
}
