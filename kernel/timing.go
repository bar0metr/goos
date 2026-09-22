//go:build uefi

package main

import (
	"goos/kernel/arch/amd64"
	"goos/kernel/clock"
	"goos/kernel/diag"
	"goos/kernel/kcall"
	"goos/kernel/mm/vm"
)

// calibrationTicks spans about 100 ms at every supported rate, long enough
// that the interrupt-entry jitter at either end is negligible.
const calibrationTicks = clock.TicksPerSecond / 10

var monotonicClock clock.Counter

// calibrateClock measures the TSC rate across calibrationTicks PIT periods.
// Both readings follow a tick delivery, so the interrupt latency cancels out.
func calibrateClock() {
	for first := amd64.TimerTicks(); amd64.TimerTicks() == first; {
		amd64.Halt()
	}
	startTick := amd64.TimerTicks()
	start := amd64.ReadTSC()
	for amd64.TimerTicks() < startTick+calibrationTicks {
		amd64.Halt()
	}
	endTick := amd64.TimerTicks()
	end := amd64.ReadTSC()
	if !monotonicClock.Calibrate(startTick, start, end, endTick-startTick) {
		amd64.NewCOM1().WriteString("CLOCK_CALIBRATION_FAILED\n")
	}
}

func nowNanos() uint64 { return monotonicClock.Nanos(amd64.ReadTSC()) }

// ClockReadNanos reports nanoseconds since the timer started. Like ClockRead it
// needs no capability.
func (handler nativeCallHandler) ClockReadNanos(address uint64, length uint64) kcall.Status {
	if length != 8 {
		return kcall.StatusInvalidArgument
	}
	if !monotonicClock.Calibrated() {
		return kcall.StatusUnsupported
	}
	var encoded [8]byte
	putUint64(encoded[:], nowNanos())
	if !vm.CopyOut(handler.root, address, encoded[:]) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
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
	launchProfile.count[stage]++
	launchProfile.total[stage] += now - began
	return now
}

func dumpTiming(out diagWriter) {
	out.text("DIAG clock")
	out.uint("tsc-hz", monotonicClock.Hz())
	out.uint("now-us", nowNanos()/1000)
	out.end()
	for stage := launchStage(0); stage < launchStageCount; stage++ {
		count, total := launchProfile.count[stage], launchProfile.total[stage]
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
