//go:build cryptoprobe_sha256

package cryptoprobe

import "crypto/sha256"

const Name = "sha256"

func Run() error {
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	sum := sha256.Sum256([]byte("abc"))
	if err := checkHex(sum[:], want); err != nil {
		return err
	}
	h := sha256.New()
	h.Write([]byte("a"))
	h.Write([]byte("bc"))
	return checkHex(h.Sum(nil), want)
}
