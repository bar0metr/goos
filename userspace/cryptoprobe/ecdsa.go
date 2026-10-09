//go:build cryptoprobe_ecdsa

package cryptoprobe

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"errors"
	"math/big"
)

const Name = "ecdsa"

func Run() error {
	// Host-generated P-256 signature fixture; not a production signing key.
	key := &ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     new(big.Int).SetBytes(fixtureHex("6b17d1f2e12c4247f8bce6e563a440f277037d812deb33a0f4a13945d898c296")),
		Y:     new(big.Int).SetBytes(fixtureHex("4fe342e2fe1a7f9b8ee7eb4a7c0f9e162bce33576b315ececbb6406837bf51f5")),
	}
	r := new(big.Int).SetBytes(fixtureHex("66075fcbb85f60c56aac541056e35cc75746e0b9ad70437fdf8109085c6857b7"))
	s := new(big.Int).SetBytes(fixtureHex("5f7debede201ccdefa417b2ad71910db231d7e8ef1f1c54b7de92748abec750f"))
	digest := sha256.Sum256([]byte("GoOS cryptoprobe fixture"))
	if !ecdsa.Verify(key, digest[:], r, s) {
		return errors.New("ECDSA rejected the P-256 fixture")
	}
	digest[0] ^= 1
	if ecdsa.Verify(key, digest[:], r, s) {
		return errors.New("ECDSA accepted a changed digest")
	}
	return nil
}
