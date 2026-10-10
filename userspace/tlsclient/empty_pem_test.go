package tlsclient

import "testing"

func TestEmptyCertificateBlockFailsWithoutPanic(t *testing.T) {
	for _, data := range []string{
		"-----BEGIN CERTIFICATE-----\n-----END CERTIFICATE-----\n",
		"-----BEGIN CERTIFICATE-----\n\n-----END CERTIFICATE-----\n",
		"-----BEGIN CERTIFICATE-----\n-----BEGIN CERTIFICATE-----\nQQ==\n-----END CERTIFICATE-----\n",
	} {
		if _, err := LoadRoots([]byte(data)); err == nil {
			t.Fatal("accepted an empty or nested certificate block")
		}
	}
}
