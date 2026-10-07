package netapi

import (
	"goos/userspace/network"
	"testing"
)

// The service sends a network.Field as a netapi.Field, so both enumerations
// must number every setting the same way.
func TestFieldsMatchNetwork(t *testing.T) {
	pairs := [...][2]uint8{
		{uint8(FieldMode), uint8(network.FieldMode)},
		{uint8(FieldGateway), uint8(network.FieldGateway)},
		{uint8(FieldTTL), uint8(network.FieldTTL)},
		{uint8(FieldARPResolveAttempts), uint8(network.FieldARPResolveAttempts)},
		{uint8(FieldEchoReplyRate), uint8(network.FieldEchoReplyRate)},
		{uint8(FieldDHCPMaxRetry), uint8(network.FieldDHCPMaxRetry)},
		{uint8(FieldProbeInterval), uint8(network.FieldProbeInterval)},
		{uint8(lastField), uint8(network.FieldTTL) + uint8(network.TuningFields) - 1},
	}
	for _, pair := range pairs {
		if pair[0] != pair[1] {
			t.Fatalf("netapi field %d, network field %d", pair[0], pair[1])
		}
	}
}
