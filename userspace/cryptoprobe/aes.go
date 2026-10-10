//go:build cryptoprobe_aes

package cryptoprobe

import "crypto/aes"

const Name = "aes"

func Run() error {
	// FIPS 197, AES-128 example.
	block, err := aes.NewCipher(fixtureHex("000102030405060708090a0b0c0d0e0f"))
	if err != nil {
		return err
	}
	var encrypted, decrypted [16]byte
	block.Encrypt(encrypted[:], fixtureHex("00112233445566778899aabbccddeeff"))
	if err := checkHex(encrypted[:], "69c4e0d86a7b0430d8cdb78070b4c55a"); err != nil {
		return err
	}
	block.Decrypt(decrypted[:], encrypted[:])
	return checkHex(decrypted[:], "00112233445566778899aabbccddeeff")
}
