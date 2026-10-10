//go:build cryptoprobe_cipher

package cryptoprobe

import (
	"crypto/aes"
	"crypto/cipher"
	"errors"
)

const Name = "cipher"

func Run() error {
	// NIST GCM: zero key, nonce, and one plaintext block.
	block, err := aes.NewCipher(make([]byte, 16))
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	nonce := make([]byte, 12)
	sealed := gcm.Seal(nil, nonce, make([]byte, 16), nil)
	if err := checkHex(sealed, "0388dace60b6a392f328c2b971b2fe78ab6e47d42cec13bdf53a67b21257bddf"); err != nil {
		return err
	}
	plain, err := gcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		return err
	}
	if err := checkHex(plain, "00000000000000000000000000000000"); err != nil {
		return err
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := gcm.Open(nil, nonce, sealed, nil); err == nil {
		return errors.New("GCM accepted a changed authentication tag")
	}
	return nil
}
