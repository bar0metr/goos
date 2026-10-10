package remoteapi

import "testing"

func testPolicy() WorkerPolicy {
	var policy WorkerPolicy
	policy.Generation, policy.MaxSessions, policy.MaxAuthAttempts = 1, 4, 3
	policy.HandshakeTimeoutMS, policy.KeyCount = 15000, 2
	policy.Seed[0], policy.Keys[0][0], policy.Keys[1][0] = 7, 11, 12
	return policy
}

func TestWorkerPolicyRoundTrip(t *testing.T) {
	policy := testPolicy()
	var page [PolicyPageSize]byte
	if !EncodePolicy(&policy, page[:]) {
		t.Fatal("encode")
	}
	var decoded WorkerPolicy
	if !DecodePolicy(page[:], &decoded) || decoded != policy {
		t.Fatal("decode")
	}
	decoded.Keys[63][0] = 99
	if !DecodePolicy(page[:], &decoded) || decoded.Keys[63][0] != 0 {
		t.Fatal("stale unused key retained")
	}
}

func TestWorkerPolicyRejectsMalformedWithoutChangingOutput(t *testing.T) {
	policy := testPolicy()
	var page [PolicyPageSize]byte
	EncodePolicy(&policy, page[:])
	for _, change := range []func([]byte){
		func(b []byte) { b[0] = 'X' },
		func(b []byte) { b[4]++ },
		func(b []byte) { b[6]++ },
		func(b []byte) { b[8] = 65 },
		func(b []byte) { b[9] = 5 },
		func(b []byte) { b[10] = 0 },
		func(b []byte) { b[11] = 1 },
		func(b []byte) { clear(b[12:16]) },
		func(b []byte) { b[19] = 1 },
		func(b []byte) { b[20] = 1 },
		func(b []byte) { clear(b[24:32]) },
		func(b []byte) { b[len(b)-1] = 1 },
	} {
		bad := page
		change(bad[:])
		before := policy
		if DecodePolicy(bad[:], &policy) || policy != before {
			t.Fatal("malformed page accepted or output changed")
		}
	}
	if DecodePolicy(page[:len(page)-1], &policy) {
		t.Fatal("truncated page accepted")
	}
}

func TestWorkerPolicyMaximumAndEmptyKeys(t *testing.T) {
	policy := testPolicy()
	var page [PolicyPageSize]byte
	for _, count := range []uint8{0, MaxKeys} {
		policy.KeyCount = count
		var decoded WorkerPolicy
		if !EncodePolicy(&policy, page[:]) || !DecodePolicy(page[:], &decoded) || decoded.KeyCount != count {
			t.Fatal("count", count)
		}
	}
}
