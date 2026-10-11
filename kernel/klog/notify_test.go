package klog

import (
	"sync"
	"testing"
)

func TestNotificationCoalescesAndNeverRetainsIdleWork(t *testing.T) {
	var notification Notification
	if notification.Take() {
		t.Fatal("new notifier has work")
	}
	for i := 0; i < 100; i++ {
		notification.Notify()
	}
	if !notification.Take() || notification.Take() {
		t.Fatal("notification did not coalesce")
	}
	if allocations := testing.AllocsPerRun(100, func() { notification.Notify(); notification.Take() }); allocations != 0 {
		t.Fatalf("allocations %v", allocations)
	}
}

func TestNotificationRacingDrainLeavesPublishedTail(t *testing.T) {
	var ring Ring
	var notification Notification
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := uint64(0); i < 5000; i++ {
			appendSequence(&ring, i)
			notification.Notify()
		}
	}()
	observed := uint64(0)
	for i := 0; i < 5000; i++ {
		if notification.Take() {
			observed = ring.Total()
		}
	}
	wg.Wait()
	if notification.Take() {
		observed = ring.Total()
	}
	if observed != 5000 || notification.Take() {
		t.Fatalf("lost tail %d", observed)
	}
}
