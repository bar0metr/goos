package kcall

import "encoding/binary"

// CPURuntimeSize is the record CPURuntime writes, laid out as:
//
//	[0:4]   processor index
//	[4]     flags: CPURuntimeOnline
//	[5:8]   reserved, zero
//	[8:16]  task the processor runs, or zero when it idles
//	[16:24] timer ticks
//	[24:32] ticks spent in the idle loop
//	[32:40] task switches
//	[40:48] tasks taken from other processors' queues
//	[48:56] tasks moved to this processor's queue
//	[56:64] reschedule interrupts received
//	[64:72] TLB shootdown interrupts received
//	[72:80] reserved, zero
const CPURuntimeSize = uint64(80)

// CPURuntimeOnline marks a processor that runs tasks.
const CPURuntimeOnline = uint8(1)

// AffinityMaskSize is the mask TaskAffinity writes: one bit per processor.
const AffinityMaskSize = uint64(8)

// CPURuntime is one processor's counters at one moment. Each processor
// counts its own, so records of two processors are not one instant.
type CPURuntime struct {
	Index       uint32
	Online      bool
	Current     uint64
	Ticks       uint64
	IdleTicks   uint64
	Switches    uint64
	Steals      uint64
	Migrations  uint64
	Reschedules uint64
	Shootdowns  uint64
}

// Encode writes the record; target must hold CPURuntimeSize bytes.
func (runtime CPURuntime) Encode(target []byte) bool {
	if uint64(len(target)) < CPURuntimeSize {
		return false
	}
	clear(target[:CPURuntimeSize])
	binary.LittleEndian.PutUint32(target[0:4], runtime.Index)
	if runtime.Online {
		target[4] = CPURuntimeOnline
	}
	for index, value := range [...]uint64{runtime.Current, runtime.Ticks, runtime.IdleTicks, runtime.Switches,
		runtime.Steals, runtime.Migrations, runtime.Reschedules, runtime.Shootdowns} {
		binary.LittleEndian.PutUint64(target[8+8*index:16+8*index], value)
	}
	return true
}

// DecodeCPURuntime reads a record Encode wrote.
func DecodeCPURuntime(source []byte) (CPURuntime, bool) {
	if uint64(len(source)) < CPURuntimeSize || source[4]&^CPURuntimeOnline != 0 {
		return CPURuntime{}, false
	}
	word := func(index int) uint64 { return binary.LittleEndian.Uint64(source[8+8*index : 16+8*index]) }
	return CPURuntime{
		Index: binary.LittleEndian.Uint32(source[0:4]), Online: source[4]&CPURuntimeOnline != 0,
		Current: word(0), Ticks: word(1), IdleTicks: word(2), Switches: word(3),
		Steals: word(4), Migrations: word(5), Reschedules: word(6), Shootdowns: word(7),
	}, true
}

// Busy returns the share of ticks the processor spent running tasks, in
// percent, and zero before its first tick.
func (runtime CPURuntime) Busy() uint64 {
	if runtime.Ticks == 0 {
		return 0
	}
	idle := min(runtime.IdleTicks, runtime.Ticks)
	return (runtime.Ticks - idle) * 100 / runtime.Ticks
}
