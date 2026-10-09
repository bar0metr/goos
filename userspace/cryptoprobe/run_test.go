//go:build cryptoprobe_sha256 || cryptoprobe_sha512 || cryptoprobe_hmac || cryptoprobe_aes || cryptoprobe_cipher || cryptoprobe_ecdh || cryptoprobe_ecdsa || cryptoprobe_rsa || cryptoprobe_asn1 || cryptoprobe_pem || cryptoprobe_x509

package cryptoprobe

import "testing"

func TestRun(t *testing.T) {
	if err := Run(); err != nil {
		t.Fatalf("%s: %v", Name, err)
	}
}
