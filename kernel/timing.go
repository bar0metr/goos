//go:build uefi

package main

import (
	"sync/atomic"

	"goos/internal/capability"
	"goos/internal/logfmt"
	"goos/internal/timekeeping"
	"goos/kernel/arch/amd64"
	"goos/kernel/clock"
	"goos/kernel/device"
	"goos/kernel/diag"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/kernel/mm/vm"
)

// calibrationTicks spans about 100 ms at every supported rate, long enough
// that the interrupt-entry jitter at either end is negligible.
const calibrationTicks = clock.TicksPerSecond / 10

var (
	monotonicClock clock.Counter
	realtimeClock  clock.Realtime
)

// calibrateClock measures the TSC rate across calibrationTicks PIT periods.
// Both readings follow a tick delivery, so the interrupt latency cancels out.
func calibrateClock() {
	for first := amd64.TimerTicks(); amd64.TimerTicks() == first; {
		amd64.Halt()
	}
	startTick := amd64.TimerTicks()
	start := amd64.ReadTSC()
	amd64.StartLocalTimerCalibration()
	for amd64.TimerTicks() < startTick+calibrationTicks {
		amd64.Halt()
	}
	localTimerElapsed = amd64.LocalTimerElapsed()
	endTick := amd64.TimerTicks()
	end := amd64.ReadTSC()
	localTimerTicks = endTick - startTick
	if !monotonicClock.Calibrate(startTick, start, end, endTick-startTick) {
		kernelLog(logfmt.Error, "CLOCK_CALIBRATION_FAILED", nil)
	}
}

// The local APIC timer runs through the same PIT periods as the TSC.
var (
	localTimerElapsed uint32
	localTimerTicks   uint64
)

// startLocalTimer moves the scheduler tick from the PIT to the bootstrap
// processor's local APIC timer, at the rate the PIT measured. Every other
// processor starts its own timer with the same count when it joins.
func startLocalTimer() bool {
	return amd64.SwitchToLocalTimer(localTimerElapsed, localTimerTicks)
}

// waitMicros spins for at least micros microseconds by the calibrated TSC.
func waitMicros(micros uint64) {
	start := nowNanos()
	for nowNanos()-start < micros*1000 {
		amd64.Pause()
	}
}

func nowNanos() uint64 { return monotonicClock.Nanos(amd64.ReadTSC()) }

// ClockReadNanos reports nanoseconds since the timer started. Like ClockRead it
// needs no capability.
func (handler nativeCallHandler) ClockReadNanos(address uint64, length uint64) kcall.Status {
	local := thisCPU()
	if length != 8 {
		return kcall.StatusInvalidArgument
	}
	if !monotonicClock.Calibrated() {
		return kcall.StatusUnsupported
	}
	putUint64(local.nanosWire[:], nowNanos())
	if !vm.CopyOut(handler.root, address, local.nanosWire[:]) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}

// ClockNow is the typed native clock API. Reading time needs no capability.
func (handler nativeCallHandler) ClockNow(clockID uint64, address uint64, length uint64) kcall.Status {
	local := thisCPU()
	if length != kcall.ClockTimeSize {
		return kcall.StatusInvalidArgument
	}
	if !monotonicClock.Calibrated() {
		return kcall.StatusUnsupported
	}
	var value timekeeping.Time
	switch timekeeping.Clock(clockID) {
	case timekeeping.Monotonic:
		value = timekeeping.FromNanos(nowNanos())
	case timekeeping.Realtime:
		var ok bool
		value, ok = realtimeClock.Now(&monotonicClock, amd64.ReadTSC())
		if !ok {
			return kcall.StatusUnsupported
		}
	default:
		return kcall.StatusInvalidArgument
	}
	clear(local.timeWire[:])
	putUint64(local.timeWire[0:8], uint64(value.Seconds))
	putUint32(local.timeWire[8:12], value.Nanoseconds)
	if !vm.CopyOut(handler.root, address, local.timeWire[:]) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}

// ClockSet is reserved for future synchronization and manual setting. The
// caller must hold the dedicated CLOCK_SET right; setting is not implemented.
func (handler nativeCallHandler) ClockSet(authorityID uint64, address uint64, length uint64) kcall.Status {
	owner, found := handler.self()
	if !found {
		return kcall.StatusPermissionDenied
	}
	authority, allowed := owner.Handles().Lookup(handle.ID(authorityID), handle.TypeDevice, capability.ClockSet)
	if !allowed || authority.Object() != device.ClockObject {
		return kcall.StatusPermissionDenied
	}
	return kcall.StatusUnsupported
}

type launchStage uint8

const (
	launchLookup launchStage = iota
	launchAddressSpace
	launchLoad
	launchSetup
	launchTask
	launchStageCount
)

var launchStageNames = [launchStageCount]string{"image-lookup", "address-space", "image-load", "process-setup", "task-install"}

// launchProfile accumulates spawnExecutable stage durations in diagnostic
// builds. Lookups that find nothing are counted too.
var launchProfile struct {
	count [launchStageCount]uint64
	total [launchStageCount]uint64
}

func launchMark() uint64 {
	if !diag.Enabled {
		return 0
	}
	return nowNanos()
}

// launchStageDone charges the time since began to stage and returns the end
// time, which begins the next stage.
func launchStageDone(stage launchStage, began uint64) uint64 {
	if !diag.Enabled {
		return 0
	}
	now := nowNanos()
	atomic.AddUint64(&launchProfile.count[stage], 1)
	atomic.AddUint64(&launchProfile.total[stage], now-began)
	return now
}

func dumpTiming(out diagWriter) {
	out.text("DIAG clock")
	out.uint("tsc-hz", monotonicClock.Hz())
	out.uint("now-us", nowNanos()/1000)
	out.end()
	for stage := launchStage(0); stage < launchStageCount; stage++ {
		count, total := atomic.LoadUint64(&launchProfile.count[stage]), atomic.LoadUint64(&launchProfile.total[stage])
		average := uint64(0)
		if count != 0 {
			average = total / count
		}
		out.text("DIAG launch")
		out.word("stage", launchStageNames[stage])
		out.uint("count", count)
		out.uint("total-us", total/1000)
		out.uint("avg-us", average/1000)
		out.end()
	}
}
