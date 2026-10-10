//go:build cryptoprobe_sha512

package cryptoprobe

import "crypto/sha512"

const Name = "sha512"

func Run() error {
	const want = "ddaf35a193617abacc417349ae20413112e6fa4e89a97ea20a9eeee64b55d39a2192992a274fc1a836ba3c23a3feebbd454d4423643ce80e2a9ac94fa54ca49f"
	sum := sha512.Sum512([]byte("abc"))
	if err := checkHex(sum[:], want); err != nil {
		return err
	}
	h := sha512.New()
	h.Write([]byte("a"))
	h.Write([]byte("bc"))
	return checkHex(h.Sum(nil), want)
}
