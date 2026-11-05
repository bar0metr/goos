package shmring

import (
	"encoding/binary"
	"sync"
	"testing"
)

func testRing(page []byte) Ring { return New(Word(page, 0), Word(page, 4), page[64:], 4, 8) }

func TestRingWrapsFillsAndBreaks(t *testing.T) {
	page := make([]byte, 64+4*8)
	r := testRing(page)
	// Start near the wrap so the indices cross 2^32.
	*Word(page, 0), *Word(page, 4) = ^uint32(1), ^uint32(1)
	for round := uint32(0); round < 10; round++ {
		for index := range uint32(4) {
			p, status := r.Reserve()
			if status != OK {
				t.Fatalf("reserve %d", status)
			}
			binary.LittleEndian.PutUint32(p, round*4+index)
			r.Publish()
		}
		if _, status := r.Reserve(); status != Full || r.Room() != 0 {
			t.Fatal("full ring took an entry")
		}
		for index := range uint32(4) {
			p, status := r.Front()
			if status != OK || binary.LittleEndian.Uint32(p) != round*4+index {
				t.Fatalf("front %d", status)
			}
			r.Consume()
		}
		if _, status := r.Front(); status != Empty || r.Pending() {
			t.Fatal("empty ring gave an entry")
		}
	}
	*Word(page, 4) = *Word(page, 0) + 5
	if _, status := r.Front(); status != Broken {
		t.Fatal("indices past the ring")
	}
	if _, status := r.Reserve(); status != Broken {
		t.Fatal("reserve on broken indices")
	}
}

// TestDoorbellLosesNoWakeup runs a producer against a consumer that sleeps
// whenever its ring looks empty: every published entry is consumed, and the
// producer rings at most once per arm.
func TestDoorbellLosesNoWakeup(t *testing.T) {
	page := make([]byte, 64+4*8)
	r := testRing(page)
	var armed, rung uint32
	bell := make(chan struct{}, 1)
	const total = 20000
	var wg sync.WaitGroup
	wg.Add(1)
	rings := 0
	go func() {
		defer wg.Done()
		for sent := 0; sent < total; {
			p, status := r.Reserve()
			if status != OK {
				continue
			}
			binary.LittleEndian.PutUint32(p, uint32(sent))
			r.Publish()
			sent++
			if ShouldRing(&armed, &rung) {
				rings++
				bell <- struct{}{}
			}
		}
	}()
	arms := 0
	for got := 0; got < total; {
		if p, status := r.Front(); status == OK {
			if binary.LittleEndian.Uint32(p) != uint32(got) {
				t.Fatalf("entry %d out of order", got)
			}
			r.Consume()
			got++
			continue
		}
		Arm(&armed)
		arms++
		if r.Pending() {
			continue
		}
		<-bell
	}
	wg.Wait()
	if rings > arms {
		t.Fatalf("%d rings for %d arms", rings, arms)
	}
}
