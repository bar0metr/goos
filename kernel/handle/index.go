package handle

import "goos/kernel/lock"

// The holder index answers which processes hold a channel endpoint, so IPC
// finds the process a message's handles move to without searching every
// table under every table's lock. Tables keep it as they insert and close
// channel handles. An endpoint is held by one process at a time but for the
// moment a launch or a transfer copies it; an entry keeps up to maxHolders
// holders, each with the number of handles it has.

const (
	indexSize  = 1024 // twice the endpoints of every channel, a power of two
	maxHolders = 4
	// tombstone marks an entry whose endpoint every holder closed, so a
	// search continues past it.
	tombstone = ^uint64(0)
)

type holderEntry struct {
	object  uint64
	holders [maxHolders]uint64
	counts  [maxHolders]uint32
}

var holderIndex struct {
	lock    lock.Spin
	entries [indexSize]holderEntry
}

func takeIndex() {
	holderIndex.lock.SetLevel(lock.LevelHandleIndex)
	holderIndex.lock.Lock()
}

func slotOf(object uint64) int { return int((object * 0x9e3779b97f4a7c15) >> 54) }

// noteHolder records that owner gained (held) or gave up (!held) one handle
// to object. A table with no owner, as in host tests, records nothing.
func noteHolder(object uint64, owner uint64, held bool) {
	if owner == 0 || object == 0 || object == tombstone {
		return
	}
	takeIndex()
	entry, free := findEntry(object)
	if entry == nil && held && free >= 0 {
		entry = &holderIndex.entries[free]
		*entry = holderEntry{object: object}
	}
	if entry != nil {
		changeHolder(entry, owner, held)
	}
	holderIndex.lock.Unlock()
}

// findEntry returns the entry of object, or nil and the first slot an insert
// can use.
func findEntry(object uint64) (*holderEntry, int) {
	free := -1
	for probe := 0; probe < indexSize; probe++ {
		slot := (slotOf(object) + probe) & (indexSize - 1)
		entry := &holderIndex.entries[slot]
		switch entry.object {
		case object:
			return entry, free
		case tombstone:
			if free < 0 {
				free = slot
			}
		case 0:
			if free < 0 {
				free = slot
			}
			return nil, free
		}
	}
	return nil, free
}

func changeHolder(entry *holderEntry, owner uint64, held bool) {
	for index := range entry.holders {
		if entry.holders[index] != owner {
			continue
		}
		if held {
			entry.counts[index]++
			return
		}
		entry.counts[index]--
		if entry.counts[index] == 0 {
			entry.holders[index] = 0
		}
		clearIfEmpty(entry)
		return
	}
	if !held {
		return
	}
	for index := range entry.holders {
		if entry.holders[index] == 0 {
			entry.holders[index], entry.counts[index] = owner, 1
			return
		}
	}
	// A fifth process holding one endpoint is not recorded; the four
	// recorded still answer for it.
}

func clearIfEmpty(entry *holderEntry) {
	for _, holder := range entry.holders {
		if holder != 0 {
			return
		}
	}
	*entry = holderEntry{object: tombstone}
}

// Holder returns a process that holds a handle to the channel endpoint
// object, or false when no table holds one.
func Holder(object uint64) (uint64, bool) {
	takeIndex()
	holder := uint64(0)
	if entry, _ := findEntry(object); entry != nil {
		for _, candidate := range entry.holders {
			if candidate != 0 {
				holder = candidate
				break
			}
		}
	}
	holderIndex.lock.Unlock()
	return holder, holder != 0
}
