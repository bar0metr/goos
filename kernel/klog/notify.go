package klog

import "sync/atomic"

// Notification coalesces append notifications without acquiring wait or
// scheduler locks in the log writer's lock context. The kernel loop drains it
// outside call/lifecycle lock sets and publishes the ring's current Total.
type Notification struct{ pending uint32 }

// Notify runs only after the ring unlocks. It never logs or acquires a lock.
func (notification *Notification) Notify() { atomic.StoreUint32(&notification.pending, 1) }

// Take clears the pending bit before observing the ring. A concurrent append
// is either included in that observation or leaves the bit set for the next
// kernel step; a reader's registration/recheck closes the same race.
func (notification *Notification) Take() bool {
	return atomic.SwapUint32(&notification.pending, 0) != 0
}
