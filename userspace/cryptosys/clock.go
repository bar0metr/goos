package cryptosys

import (
	"goos/internal/timekeeping"
	"goos/kernel/kcall"
	"goos/userspace/runtime/sys"
	"time"
)

type clockError struct{}

func (clockError) Error() string { return "realtime clock unavailable or invalid" }

// Now returns UTC from the native REALTIME source, without a monotonic fallback.
func Now() (time.Time, error) {
	value, result := sys.ClockNow(timekeeping.Realtime)
	if result != kcall.StatusOK || value.Seconds <= 0 || value.Seconds > 253402300799 || value.Nanoseconds >= 1000000000 {
		return time.Time{}, clockError{}
	}
	return time.Unix(value.Seconds, int64(value.Nanoseconds)).UTC(), nil
}
