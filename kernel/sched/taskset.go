package sched

import "math/bits"

// TaskWords is the number of 64-bit words in a TaskSet.
const TaskWords = MaxTasks / 64

// TaskSet holds one bit per task slot, slot s in bit s%64 of word s/64. The
// scheduler keeps its run, dead, and exit-pending sets as TaskSets and scans
// them a word at a time: a search costs at most TaskWords trailing-zero counts.
//
// The methods that change a set take a pointer; the ones that combine sets
// return a new set by value, which is TaskWords words and never escapes.
type TaskSet [TaskWords]uint64

// Has reports whether slot is in the set. A slot out of range is not.
func (set *TaskSet) Has(slot int) bool {
	if slot < 0 || slot >= MaxTasks {
		return false
	}
	return set[slot>>6]&(uint64(1)<<uint(slot&63)) != 0
}

// Add puts slot in the set.
func (set *TaskSet) Add(slot int) {
	if slot >= 0 && slot < MaxTasks {
		set[slot>>6] |= uint64(1) << uint(slot&63)
	}
}

// Remove takes slot out of the set.
func (set *TaskSet) Remove(slot int) {
	if slot >= 0 && slot < MaxTasks {
		set[slot>>6] &^= uint64(1) << uint(slot&63)
	}
}

// Empty reports whether the set holds no slot.
func (set *TaskSet) Empty() bool {
	for word := 0; word < TaskWords; word++ {
		if set[word] != 0 {
			return false
		}
	}
	return true
}

// Count returns the number of slots in the set.
func (set *TaskSet) Count() int {
	count := 0
	for word := 0; word < TaskWords; word++ {
		count += bits.OnesCount64(set[word])
	}
	return count
}

// First returns the lowest slot in the set, or -1 for an empty set.
func (set *TaskSet) First() int {
	for word := 0; word < TaskWords; word++ {
		if set[word] != 0 {
			return word<<6 + bits.TrailingZeros64(set[word])
		}
	}
	return -1
}

// NextAfter returns the lowest slot above after, wrapping to the lowest slot
// in the set, or -1 for an empty set. An after outside [0, MaxTasks) starts
// the search at slot 0.
func (set *TaskSet) NextAfter(after int) int {
	if after >= 0 && after < MaxTasks-1 {
		start := after + 1
		word := start >> 6
		if above := set[word] &^ (uint64(1)<<uint(start&63) - 1); above != 0 {
			return word<<6 + bits.TrailingZeros64(above)
		}
		for word++; word < TaskWords; word++ {
			if set[word] != 0 {
				return word<<6 + bits.TrailingZeros64(set[word])
			}
		}
	}
	return set.First()
}

// Intersects reports whether the two sets share a slot.
func (set *TaskSet) Intersects(other *TaskSet) bool {
	for word := 0; word < TaskWords; word++ {
		if set[word]&other[word] != 0 {
			return true
		}
	}
	return false
}

// SubsetOf reports whether every slot of set is in other.
func (set *TaskSet) SubsetOf(other *TaskSet) bool {
	for word := 0; word < TaskWords; word++ {
		if set[word]&^other[word] != 0 {
			return false
		}
	}
	return true
}

// Union adds the slots of other to the set.
func (set *TaskSet) Union(other *TaskSet) {
	for word := 0; word < TaskWords; word++ {
		set[word] |= other[word]
	}
}

// And returns the slots in both sets.
func (set TaskSet) And(other TaskSet) TaskSet {
	for word := 0; word < TaskWords; word++ {
		set[word] &= other[word]
	}
	return set
}

// AndNot returns the slots of set that are not in other.
func (set TaskSet) AndNot(other TaskSet) TaskSet {
	for word := 0; word < TaskWords; word++ {
		set[word] &^= other[word]
	}
	return set
}

// Or returns the slots in either set.
func (set TaskSet) Or(other TaskSet) TaskSet {
	for word := 0; word < TaskWords; word++ {
		set[word] |= other[word]
	}
	return set
}

// lowTasks returns the set of slots [0, count).
func lowTasks(count int) TaskSet {
	var set TaskSet
	for word := 0; word < TaskWords && count > 0; word++ {
		if count >= 64 {
			set[word] = ^uint64(0)
		} else {
			set[word] = uint64(1)<<uint(count) - 1
		}
		count -= 64
	}
	return set
}
