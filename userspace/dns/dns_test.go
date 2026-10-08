package dns

import "testing"

func TestDefaultPolicy(t *testing.T) {
	policy := DefaultPolicy()
	want := Policy{CacheEntries: 64, MaxTTL: 86400, MaxNegative: 3600, FailureTTL: 5,
		QueryTimeout: 3000 * millisecond, Attempts: 2}
	if policy != want || policy.Validate() != -1 {
		t.Fatalf("%+v", policy)
	}
}

func TestParsePolicy(t *testing.T) {
	good := [PolicyFields]uint64{64, 86400, 3600, 5, 3000, 2}
	policy, bad := ParsePolicy(&good)
	if bad != -1 || policy != DefaultPolicy() {
		t.Fatalf("%+v %d", policy, bad)
	}
	low := [PolicyFields]uint64{1, 1, 1, 1, 500, 1}
	high := [PolicyFields]uint64{256, 604800, 86400, 300, 30000, 3}
	for _, values := range [][PolicyFields]uint64{low, high} {
		policy, bad := ParsePolicy(&values)
		if bad != -1 || policy.Validate() != -1 {
			t.Fatalf("%v: %+v %d", values, policy, bad)
		}
	}
	for index := 0; index < PolicyFields; index++ {
		// Values that would wrap into range when narrowed, or multiplied.
		for _, value := range []uint64{0, low[index] - 1, high[index] + 1, 1 << 16, 1<<32 + 64, 1<<63 + 3000,
			^uint64(0)} {
			if value >= low[index] && value <= high[index] {
				continue
			}
			values := good
			values[index] = value
			if _, bad := ParsePolicy(&values); bad != index {
				t.Fatalf("field %d value %d: bad %d", index, value, bad)
			}
		}
	}
	// The first bad value is reported.
	values := good
	values[2], values[4] = 0, 0
	if _, bad := ParsePolicy(&values); bad != 2 {
		t.Fatalf("bad %d", bad)
	}
}

func TestPolicyValidate(t *testing.T) {
	policy := DefaultPolicy()
	policy.QueryTimeout = 499 * millisecond
	if policy.Validate() != 4 {
		t.Fatal("short timeout accepted")
	}
	policy = DefaultPolicy()
	policy.Attempts = 4
	if policy.Validate() != 5 {
		t.Fatal("four attempts accepted")
	}
	policy = DefaultPolicy()
	policy.CacheEntries = MaxCacheEntries + 1
	if policy.Validate() != 0 {
		t.Fatal("oversized cache accepted")
	}
}

func TestParseServers(t *testing.T) {
	cases := []struct {
		texts [4]string
		want  [4][4]byte
		bad   int
	}{
		{[4]string{"192.0.2.1", "0.0.0.0", "", "198.51.100.7"},
			[4][4]byte{{192, 0, 2, 1}, {}, {}, {198, 51, 100, 7}}, -1},
		{[4]string{"0.0.0.0", "0.0.0.0", "0.0.0.0", "0.0.0.0"}, [4][4]byte{}, -1},
		{[4]string{"192.0.2.1", "192.0.2.01"}, [4][4]byte{}, 1},
		{[4]string{"192.0.2.1", "192.0.2"}, [4][4]byte{}, 1},
		{[4]string{"256.0.0.1"}, [4][4]byte{}, 0},
		{[4]string{" 1.2.3.4"}, [4][4]byte{}, 0},
		{[4]string{"1.2.3.4.5"}, [4][4]byte{}, 0},
		{[4]string{"1.1.1.1", "255.255.255.255"}, [4][4]byte{}, 1},
		{[4]string{"224.0.0.1"}, [4][4]byte{}, 0},
		{[4]string{"240.0.0.1"}, [4][4]byte{}, 0},
		{[4]string{"127.0.0.53"}, [4][4]byte{}, 0},
		{[4]string{"0.1.2.3"}, [4][4]byte{}, 0},
		{[4]string{"8.8.8.8", "0.0.0.0", "8.8.8.8"}, [4][4]byte{}, 2},
	}
	for _, c := range cases {
		var texts [4][]byte
		for index, text := range c.texts {
			texts[index] = []byte(text)
		}
		got, bad := ParseServers(&texts)
		if got != c.want || bad != c.bad {
			t.Fatalf("%q: %v %d, want %v %d", c.texts, got, bad, c.want, c.bad)
		}
	}
}
