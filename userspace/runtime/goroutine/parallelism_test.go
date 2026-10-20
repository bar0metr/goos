package goroutine

import (
	"testing"

	"goos/userspace/runtime/tls"
)

func TestParallelismHonorsCPUQuotaAndTCBBounds(t *testing.T) {
	for _, sample := range []struct{ online, quota, want int32 }{
		{1, 8, 1}, {2, 8, 2}, {8, 4, 4}, {64, 1, 1},
		{64, 8, 8}, {64, 64, tls.Slots}, {0, 8, 1}, {8, 0, 1},
	} {
		if got := parallelism(sample.online, sample.quota); got != sample.want {
			t.Fatalf("online=%d quota=%d: %d want %d", sample.online, sample.quota, got, sample.want)
		}
	}
}
