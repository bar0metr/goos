package linux

import "goos/kernel/compat/linux/abi"

// MaxEventCounters bounds the eventfd counters of all families together.
const MaxEventCounters = 64

// eventCounterMax is the largest value an eventfd counter holds, 2^64-2:
// a write that would pass it waits, and a write of 2^64-1 itself is
// EINVAL, as on Linux.
const eventCounterMax = ^uint64(0) - 1

// eventCounter is one eventfd object. Its descriptions count references in
// the descriptor tables; the counter goes with the last of them.
type eventCounter struct {
	familyID   uint64
	generation uint32
	value      uint64
	semaphore  bool
}

var (
	eventCounters         [MaxEventCounters]eventCounter
	nextEventGeneration   = uint32(1)
	eventCounterObjectBit = uint64(1) << 62
)

// The Object of an FDEvent description: a marker bit, the generation, and
// the table index, so a reused slot never matches an old description.
func eventObject(index int, generation uint32) uint64 {
	return eventCounterObjectBit | uint64(generation)<<16 | uint64(index)
}

func eventSlotHeld(familyID uint64, object uint64) (*eventCounter, bool) {
	guard.Check()
	index := int(object & 0xffff)
	if object&eventCounterObjectBit == 0 || index >= len(eventCounters) {
		return nil, false
	}
	counter := &eventCounters[index]
	if counter.familyID == 0 || counter.familyID != familyID || counter.generation != uint32(object>>16) {
		return nil, false
	}
	return counter, true
}

// CreateEventCounter makes a counter for familyID holding initial and
// returns the Object of its description. ENFILE when the table is full.
func CreateEventCounter(familyID uint64, initial uint32, semaphore bool) (uint64, abi.Errno) {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	if familyID == 0 {
		return 0, abi.EINVAL
	}
	for index := range eventCounters {
		counter := &eventCounters[index]
		if counter.familyID != 0 {
			continue
		}
		generation := nextEventGeneration
		nextEventGeneration++
		if nextEventGeneration == 0 || nextEventGeneration > 0xffffffff>>2 {
			nextEventGeneration = 1
		}
		*counter = eventCounter{familyID: familyID, generation: generation, value: uint64(initial), semaphore: semaphore}
		return eventObject(index, generation), 0
	}
	return 0, abi.ENFILE
}

// ReadEventCounter takes the counter's value, or 1 of it for a semaphore,
// and reports whether it had any. valid is false for a counter that is gone.
func ReadEventCounter(familyID uint64, object uint64) (value uint64, ready bool, valid bool) {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	counter, found := eventSlotHeld(familyID, object)
	if !found {
		return 0, false, false
	}
	if counter.value == 0 {
		return 0, false, true
	}
	if counter.semaphore {
		counter.value--
		return 1, true, true
	}
	value, counter.value = counter.value, 0
	return value, true, true
}

// WriteEventCounter adds add to the counter, or reports that it does not
// fit yet. A write of 2^64-1 is EINVAL.
func WriteEventCounter(familyID uint64, object uint64, add uint64) (ready bool, errno abi.Errno) {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	counter, found := eventSlotHeld(familyID, object)
	if !found {
		return false, abi.EBADF
	}
	if add == ^uint64(0) {
		return false, abi.EINVAL
	}
	if add > eventCounterMax-counter.value {
		return false, 0
	}
	counter.value += add
	return true, 0
}

// EventCounterReady reports what a read and a write could do now: read
// while the value is nonzero, write while one more unit fits.
func EventCounterReady(familyID uint64, object uint64) (readable bool, writable bool, valid bool) {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	counter, found := eventSlotHeld(familyID, object)
	if !found {
		return false, false, false
	}
	return counter.value != 0, counter.value < eventCounterMax, true
}

// ReleaseEventCounter frees a counter whose last description closed.
func ReleaseEventCounter(familyID uint64, object uint64) {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	if counter, found := eventSlotHeld(familyID, object); found {
		*counter = eventCounter{}
	}
}

// ReleaseEventCounterObject frees a counter by its Object alone, which is
// unique: the last description of it closed, in whatever process.
func ReleaseEventCounterObject(object uint64) {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	index := int(object & 0xffff)
	if object&eventCounterObjectBit == 0 || index >= len(eventCounters) {
		return
	}
	if counter := &eventCounters[index]; counter.familyID != 0 && counter.generation == uint32(object>>16) {
		*counter = eventCounter{}
	}
}

// releaseEventCountersHeld frees what a family that ended left.
func releaseEventCountersHeld(familyID uint64) {
	guard.Check()
	for index := range eventCounters {
		if eventCounters[index].familyID == familyID {
			eventCounters[index] = eventCounter{}
		}
	}
}

// EventStatus is the synthetic fstat record of an eventfd, an anonymous
// inode with mode 0600 and no file type, as Linux reports it.
func EventStatus(object uint64) FileStatus {
	return FileStatus{Device: StreamDevice, Inode: object &^ eventCounterObjectBit, Mode: 0o600}
}
