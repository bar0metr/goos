//go:build cryptoprobe_rsa

package cryptoprobe

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"errors"
	"math/big"
)

const Name = "rsa"

func Run() error {
	// Fixed host-generated RSA-2048 PSS fixture with a 32-byte salt.
	key := &rsa.PublicKey{
		N: new(big.Int).SetBytes(fixtureHex("b59eefb18164e6e01a94ce29b4d782b512904f653a1d46a7bf7e6b600216a07e2e5533b354bf9cce3fe059194afb7c5ba892d42a88461a30113548af82e2b77db54256e7e5c2c0037523b42b2d13d28230c7a0c02170b3d552491b90c20341214887816dc7b9772f1c32874d08ef6b735d1b83c41abc2f22e374d2d138a19513abf32db27379af26f5a549c64fd79fcc1ef6eacc6fae216e63be8111872bfd7bb6006fd9a6a666eeb006fd9a98172a540d0975fab92e1c5ed775d3cb5e4560e5d5d24c98aef1c5c27773bd9df8b4e2e4e9147288e02b7987789ead3da9218b5e183c35a7caefb534010520b037542b378c25b4fb3bdc6f5863922520dbf129c5")),
		E: 65537,
	}
	signature := fixtureHex("747f015f10999adbe4fed76bbc86d3f2072eceacb527de5815138d187b1fa0fb7277a86da9d5697acc7cce49b1ede3468df0e1587ef884caa14ad24edfdc165362d9f9abb3060bc5f73acad511ebed387fa4ed583c40a2ff9261c17f51b179978b3c97127ba26d7b53cb94d0db1b9b0834dda378844a16f6205de2a04f8f96861933584a74b4a6927e3bba9c9e05c9463057559723c7ad63ac6018f643872470cbb5df009523450ba39e92ccf3d0e31907deeb1aea58f8284bd7d047adf52777fbcd82e36e997812ddce8738a515b5cbf2c0607424bce35784253b152a04bdbd428a8c934feb2a6fbfddcc06332d4c0b97cc6f566360ee5aa8acbb8d5baf63ab")
	digest := sha256.Sum256([]byte("GoOS cryptoprobe fixture"))
	options := &rsa.PSSOptions{SaltLength: 32, Hash: crypto.SHA256}
	if err := rsa.VerifyPSS(key, crypto.SHA256, digest[:], signature, options); err != nil {
		return err
	}
	signature[0] ^= 1
	if err := rsa.VerifyPSS(key, crypto.SHA256, digest[:], signature, options); err == nil {
		return errors.New("RSA-PSS accepted a changed signature")
	}
	return nil
}
