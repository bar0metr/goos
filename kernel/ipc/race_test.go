package ipc

import (
	"encoding/binary"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These tests run channels the way processors share them (ADR 0187): each
// channel under its own lock, the registry under its slots lock. A barrier
// starts the racing goroutines together, and a deadline turns a deadlock
// into a failure.

const raceDeadline = 10 * time.Second

func finish(t *testing.T, group *sync.WaitGroup) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		group.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(raceDeadline):
		t.Fatal("racing goroutines did not finish")
	}
}

func numbered(sequence uint64) *Message {
	message := &Message{ProtocolID: 1, Version: 1, Kind: KindNotification, PayloadSize: 8}
	binary.LittleEndian.PutUint64(message.Payload[:8], sequence)
	return message
}

// Traffic on one channel goes on while another channel's lock is held.
func TestChannelTrafficWhileAnotherIsBusy(t *testing.T) {
	var registry Registry
	_, busyID, _ := registry.Create(1)
	_, freeID, _ := registry.Create(1)
	busy, _ := registry.Acquire(busyID)
	var group sync.WaitGroup
	group.Add(1)
	go func() {
		defer group.Done()
		for round := uint64(1); round <= 1000; round++ {
			channel, live := registry.Acquire(freeID)
			if !live || channel.SendFrom(0, numbered(round), nil, nil, nil) != StatusOK {
				t.Error("send on the free channel failed")
				return
			}
			var received Message
			if channel.ReceiveInto(1, &received) != StatusOK || binary.LittleEndian.Uint64(received.Payload[:8]) != round {
				t.Error("receive on the free channel failed")
			}
			channel.Unlock()
			if _, extra, created := registry.Create(1); !created || !registry.Live(extra) {
				t.Error("the registry stopped while a channel was held")
			} else {
				registry.Close(extra, 0)
				registry.Close(extra, 1)
			}
		}
	}()
	finish(t, &group)
	busy.Unlock()
}

// A sender, a receiver, and two closers race on one channel. Every message
// the sender queued arrives once, in order; the receiver then sees the
// peer's death; exactly one closer closes the side.
func TestCloseRacesSendAndReceive(t *testing.T) {
	for round := 0; round < 50; round++ {
		var registry Registry
		_, id, _ := registry.Create(1)
		var sent []uint64
		var received []uint64
		var closes atomic.Int32
		start := make(chan struct{})
		midway := make(chan struct{})
		var midwayOnce sync.Once
		var group sync.WaitGroup
		group.Add(4)
		go func() {
			defer group.Done()
			<-start
			for sequence := uint64(1); ; sequence++ {
				if sequence == 40 {
					midwayOnce.Do(func() { close(midway) })
				}
				channel, live := registry.Acquire(id)
				if !live {
					return
				}
				status := channel.SendFrom(0, numbered(sequence), nil, nil, nil)
				channel.Unlock()
				switch status {
				case StatusOK:
					sent = append(sent, sequence)
				case StatusResourceExhausted:
					sequence--
				default:
					if sequence < 40 {
						midwayOnce.Do(func() { close(midway) })
					}
					return
				}
			}
		}()
		go func() {
			defer group.Done()
			<-start
			var message Message
			for {
				channel, live := registry.Acquire(id)
				if !live {
					t.Error("the channel went away before its receiver closed")
					return
				}
				status := channel.ReceiveInto(1, &message)
				channel.Unlock()
				if status == StatusPeerDied {
					return
				}
				if status == StatusOK {
					received = append(received, binary.LittleEndian.Uint64(message.Payload[:8]))
				}
			}
		}()
		for closer := 0; closer < 2; closer++ {
			go func() {
				defer group.Done()
				<-midway
				if closed, _, released := registry.Close(id, 0); closed {
					closes.Add(1)
					if released {
						t.Error("closing one side released the channel")
					}
				}
			}()
		}
		close(start)
		finish(t, &group)
		if closes.Load() != 1 {
			t.Fatalf("round %d: side closed %d times", round, closes.Load())
		}
		if len(received) != len(sent) {
			t.Fatalf("round %d: sent %d, received %d", round, len(sent), len(received))
		}
		for index := range sent {
			if received[index] != sent[index] {
				t.Fatalf("round %d: message %d is %d, want %d", round, index, received[index], sent[index])
			}
		}
		if closed, owner, released := registry.Close(id, 1); !closed || !released || owner != 1 {
			t.Fatalf("round %d: last close %v %v %d", round, closed, released, owner)
		}
	}
}

// watchedStorage records when a channel's memory goes back.
type watchedStorage struct{ freed atomic.Bool }

func (storage *watchedStorage) New() (*Channel, bool) { return new(Channel), true }
func (storage *watchedStorage) Free(*Channel)         { storage.freed.Store(true) }

// The close that releases a channel waits for the processor holding the
// channel's lock, so the memory never goes while someone uses it.
func TestReleaseWaitsForLockHolder(t *testing.T) {
	storage := &watchedStorage{}
	var registry Registry
	registry.SetStorage(storage)
	_, id, _ := registry.Create(1)
	registry.Close(id, 0)
	holder, _ := registry.Acquire(id)
	var group sync.WaitGroup
	group.Add(1)
	go func() {
		defer group.Done()
		if closed, _, released := registry.Close(id, 1); !closed || !released {
			t.Error("the last close did not release the channel")
		}
	}()
	time.Sleep(50 * time.Millisecond)
	if storage.freed.Load() {
		t.Fatal("the channel was freed while its lock was held")
	}
	if holder.Queued(1) != 0 {
		t.Fatal("the held channel changed")
	}
	holder.Unlock()
	finish(t, &group)
	if !storage.freed.Load() || registry.Live(id) {
		t.Fatal("the channel was not released")
	}
}
