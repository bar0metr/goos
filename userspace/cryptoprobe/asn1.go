//go:build cryptoprobe_asn1

package cryptoprobe

import (
	"encoding/asn1"
	"errors"
)

const Name = "asn1"

func Run() error {
	type value struct {
		Number int
		Text   string
	}
	encoded, err := asn1.Marshal(value{Number: 42, Text: "probe"})
	if err != nil {
		return err
	}
	if err := checkHex(encoded, "300a02012a130570726f6265"); err != nil {
		return err
	}
	var decoded value
	rest, err := asn1.Unmarshal(fixtureHex("300a02012a130570726f6265"), &decoded)
	if err != nil {
		return err
	}
	if len(rest) != 0 || decoded.Number != 42 || decoded.Text != "probe" {
		return errors.New("ASN.1 decoded value mismatch")
	}
	if _, err := asn1.Unmarshal([]byte{0x30, 0x02, 0x02}, &decoded); err == nil {
		return errors.New("ASN.1 accepted truncated DER")
	}
	return nil
}
