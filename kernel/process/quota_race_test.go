package process

import (
	"sync"
	"sync/atomic"
	"testing"
)

// Charges from several processors never pass the limit together, and
// releases bring the account back to zero.
func TestQuotaHoldsUnderConcurrentCharges(t *testing.T) {
	owner, _ := New(2, ABINative, AddressSpace{Root: 0x1000})
	_, limits := owner.Quota()
	limit := limits[ResourceChannels]
	var granted atomic.Int32
	var group sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for attempt := uint32(0); attempt < limit; attempt++ {
				if owner.Charge(ResourceChannels, 1) {
					granted.Add(1)
				}
			}
		}()
	}
	group.Wait()
	if uint32(granted.Load()) != limit || owner.Available(ResourceChannels) != 0 {
		t.Fatalf("granted %d of %d, %d left", granted.Load(), limit, owner.Available(ResourceChannels))
	}
	for worker := 0; worker < 8; worker++ {
		group.Add(1)
		go func(count int32) {
			defer group.Done()
			for index := int32(0); index < count; index++ {
				owner.Release(ResourceChannels, 1)
			}
		}(granted.Load()/8 + map[bool]int32{true: granted.Load() % 8, false: 0}[worker == 0])
	}
	group.Wait()
	if used, _ := owner.Quota(); used[ResourceChannels] != 0 {
		t.Fatalf("%d still charged", used[ResourceChannels])
	}
}
