//go:build uefi && amd64

package amd64

import (
	"math/bits"

	"goos/kernel/sched"
)

// Wake latency, kept only in the diagnostic build: the TSC cycles from the
// wake that makes a task runnable to the switch onto it, as a histogram of
// powers of two. Both ends run under the scheduler lock.

// WakeBuckets is the histogram's size: bucket i counts latencies below
// 2^(i+1) cycles, the last one everything longer.
const WakeBuckets = 40

// WakeLatency is a copy of the histogram.
type WakeLatency struct {
	Buckets [WakeBuckets]uint64
	Count   uint64
	Cycles  uint64
}

var (
	wakeStamps  [sched.MaxTasks]uint64
	wakeLatency WakeLatency
	// runCycles is each task slot's time on a processor; runStart the
	// TSC when each processor last switched.
	runCycles [sched.MaxTasks]uint64
	runStart  [MaxCPUs]uint64
)

// noteRunSwitch charges the time since the processor last switched to the
// task it leaves.
func noteRunSwitch(cpu int, previous int) {
	now := ReadTSC()
	if previous >= 0 && previous < len(runCycles) && runStart[cpu] != 0 {
		runCycles[previous] += now - runStart[cpu]
	}
	runStart[cpu] = now
}

// TaskRunCycles is a task slot's time on a processor, in TSC cycles.
func TaskRunCycles(slot int) uint64 {
	if slot < 0 || slot >= len(runCycles) {
		return 0
	}
	return runCycles[slot]
}

func noteWakeStart(slot int) {
	if slot >= 0 && slot < len(wakeStamps) && wakeStamps[slot] == 0 {
		wakeStamps[slot] = ReadTSC()
	}
}

func noteWakeRun(slot int) {
	if slot < 0 || slot >= len(wakeStamps) || wakeStamps[slot] == 0 {
		return
	}
	cycles := ReadTSC() - wakeStamps[slot]
	wakeStamps[slot] = 0
	bucket := int(cycles)
	if cycles >= 8 {
		power := bits.Len64(cycles) - 1
		bucket = power<<2 | int(cycles>>uint(power-2)&3)
	}
	if bucket >= WakeBuckets {
		bucket = WakeBuckets - 1
	}
	wakeLatency.Buckets[bucket]++
	wakeLatency.Count++
	wakeLatency.Cycles += cycles
}

// WakeBucketBound is the first latency, in cycles, past bucket.
func WakeBucketBound(bucket int) uint64 {
	if bucket < 8 {
		return uint64(bucket) + 1
	}
	power, quarter := uint(bucket>>2), uint64(bucket&3)
	return (4 + quarter + 1) << (power - 2)
}

// ReadWakeLatency copies the histogram.
func ReadWakeLatency() WakeLatency {
	flags := lockScheduler()
	result := wakeLatency
	unlockScheduler(flags)
	return result
}
