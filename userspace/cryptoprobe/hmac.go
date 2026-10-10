//go:build cryptoprobe_hmac

package cryptoprobe

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
)

const Name = "hmac"

func Run() error {
	// RFC 4231, test case 1.
	key := make([]byte, 20)
	for i := range key {
		key[i] = 0x0b
	}
	h := hmac.New(sha256.New, key)
	h.Write([]byte("Hi There"))
	sum := h.Sum(nil)
	if err := checkHex(sum, "b0344c61d8db38535ca8afceaf0bf12b881dc200c9833da726e9376c2e32cff7"); err != nil {
		return err
	}
	if !hmac.Equal(sum, h.Sum(nil)) {
		return errors.New("HMAC equality rejected matching MAC")
	}
	bad := append([]byte(nil), sum...)
	bad[0] ^= 1
	if hmac.Equal(sum, bad) {
		return errors.New("HMAC equality accepted changed MAC")
	}
	return nil
}
