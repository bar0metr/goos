//go:build cryptoprobe_ed25519

package cryptoprobe

import (
	"crypto/ed25519"
	"errors"
)

const Name = "ed25519"

func Run() error {
	// RFC 8032 section 7.1, empty-message signature.
	public := fixtureHex("d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a")
	signature := fixtureHex("e5564300c360ac729086e2cc806e828a84877f1eb8e5d974d873e065224901555fb8821590a33bacc61e39701cf9b46bd25bf5f0595bbe24655141438e7a100b")
	if !ed25519.Verify(ed25519.PublicKey(public), nil, signature) {
		return errors.New("Ed25519 known signature rejected")
	}
	signature[0] ^= 1
	if ed25519.Verify(ed25519.PublicKey(public), nil, signature) {
		return errors.New("Ed25519 altered signature accepted")
	}
	signature[0] ^= 1
	if ed25519.Verify(ed25519.PublicKey(public), []byte("changed"), signature) {
		return errors.New("Ed25519 altered message accepted")
	}
	public[0] ^= 1
	if ed25519.Verify(ed25519.PublicKey(public), nil, signature) {
		return errors.New("Ed25519 wrong key accepted")
	}
	return nil
}
