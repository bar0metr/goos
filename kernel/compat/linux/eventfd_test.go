package linux

import (
	"testing"

	"goos/kernel/compat/linux/abi"
)

func TestEventCounterReadsAndWrites(t *testing.T) {
	object, errno := CreateEventCounter(7, 3, false)
	if errno != 0 {
		t.Fatalf("create %d", errno)
	}
	defer ReleaseEventCounter(7, object)
	if readable, writable, valid := EventCounterReady(7, object); !readable || !writable || !valid {
		t.Fatal("initial value not readable")
	}
	if ready, errno := WriteEventCounter(7, object, 4); !ready || errno != 0 {
		t.Fatal("write")
	}
	if value, ready, _ := ReadEventCounter(7, object); !ready || value != 7 {
		t.Fatalf("read %d", value)
	}
	if _, ready, valid := ReadEventCounter(7, object); ready || !valid {
		t.Fatal("empty counter read")
	}
	if _, errno := WriteEventCounter(7, object, ^uint64(0)); errno != abi.EINVAL {
		t.Fatal("2^64-1 accepted")
	}
	if ready, _ := WriteEventCounter(7, object, eventCounterMax); !ready {
		t.Fatal("largest value refused")
	}
	if ready, errno := WriteEventCounter(7, object, 1); ready || errno != 0 {
		t.Fatal("overflow did not wait")
	}
	if _, writable, _ := EventCounterReady(7, object); writable {
		t.Fatal("full counter writable")
	}
}

func TestEventCounterSemaphoreAndOwnership(t *testing.T) {
	object, _ := CreateEventCounter(7, 2, true)
	if value, ready, _ := ReadEventCounter(7, object); !ready || value != 1 {
		t.Fatalf("semaphore read %d", value)
	}
	if _, _, valid := ReadEventCounter(8, object); valid {
		t.Fatal("another family read the counter")
	}
	ReleaseEventCounter(7, object)
	if _, _, valid := EventCounterReady(7, object); valid {
		t.Fatal("released counter still valid")
	}
	again, _ := CreateEventCounter(7, 0, false)
	defer ReleaseEventCounter(7, again)
	if again == object {
		t.Fatal("reused slot kept its identity")
	}
}

func TestEventCountersAreBounded(t *testing.T) {
	var objects []uint64
	for {
		object, errno := CreateEventCounter(9, 0, false)
		if errno != 0 {
			if errno != abi.ENFILE || len(objects) != MaxEventCounters {
				t.Fatalf("stopped at %d with %d", len(objects), errno)
			}
			break
		}
		objects = append(objects, object)
	}
	tablesLock.Lock()
	releaseEventCountersHeld(9)
	tablesLock.Unlock()
	if _, errno := CreateEventCounter(9, 0, false); errno != 0 {
		t.Fatal("family release left counters")
	}
	tablesLock.Lock()
	releaseEventCountersHeld(9)
	tablesLock.Unlock()
}
