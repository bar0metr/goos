//go:build cryptoprobe_ed25519 || cryptoprobe_sha256 || cryptoprobe_sha512 || cryptoprobe_hmac || cryptoprobe_aes || cryptoprobe_cipher || cryptoprobe_ecdh || cryptoprobe_ecdsa || cryptoprobe_rsa || cryptoprobe_asn1 || cryptoprobe_pem || cryptoprobe_x509 || cryptoprobe_rand || cryptoprobe_tls

package cryptoprobe

import "errors"

// Fixture decoding runs inside Run; userspace startup skips package initializers.
func fixtureHex(s string) []byte {
	b := make([]byte, len(s)/2)
	for i := range b {
		b[i] = fixtureNibble(s[2*i])<<4 | fixtureNibble(s[2*i+1])
	}
	return b
}

func fixtureNibble(c byte) byte {
	if c >= '0' && c <= '9' {
		return c - '0'
	}
	return c - 'a' + 10
}

func checkHex(got []byte, want string) error {
	if len(got)*2 != len(want) {
		return errors.New("known-answer length mismatch")
	}
	for i, b := range got {
		if b != fixtureNibble(want[2*i])<<4|fixtureNibble(want[2*i+1]) {
			return errors.New("known-answer mismatch")
		}
	}
	return nil
}
