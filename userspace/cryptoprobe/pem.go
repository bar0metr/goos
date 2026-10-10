//go:build cryptoprobe_pem

package cryptoprobe

import (
	"encoding/pem"
	"errors"
)

const Name = "pem"

func Run() error {
	const fixture = "-----BEGIN PROBE-----\nAQIDBA==\n-----END PROBE-----\n"
	block, rest := pem.Decode([]byte(fixture))
	if block == nil || block.Type != "PROBE" || len(rest) != 0 {
		return errors.New("PEM decode failed")
	}
	if err := checkHex(block.Bytes, "01020304"); err != nil {
		return err
	}
	if string(pem.EncodeToMemory(block)) != fixture {
		return errors.New("PEM encode mismatch")
	}
	if block, _ := pem.Decode([]byte("-----BEGIN PROBE-----\n!\n-----END PROBE-----\n")); block != nil {
		return errors.New("PEM accepted malformed base64")
	}
	return nil
}
