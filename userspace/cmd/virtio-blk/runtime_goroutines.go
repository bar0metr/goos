//go:build goos_userspace && goos_goroutines

package main

import (
	"runtime"
	"sync/atomic"

	"goos/internal/logfmt"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/goroutine"
	"goos/userspace/runtime/sys"
)

// The validation build runs the driver on the goroutine runtime (ADR 0204),
// so sys.IRQWait parks the serving goroutine on the disk line instead of
// blocking its thread. A peer goroutine counts on the only permit during the
// first irqProofWaits waits that park; it can run only while the server is
// parked.
const irqProofWaits = 4

var (
	// irqKick starts the peer for one wait; closing it ends the peer.
	irqKick chan struct{}
	// irqWaiting holds the peer in its counting loop.
	irqWaiting atomic.Bool
	// irqParked records that the peer saw the wait registered with the
	// poller.
	irqParked atomic.Bool
	irqSpins  atomic.Uint64
	// irqTries, irqProven and irqCounted belong to the serving goroutine.
	irqTries, irqProven, irqCounted uint64
)

func initRuntime() {
	goroutine.Init()
	arena.InitService()
	runtime.GOMAXPROCS(1)
	irqKick = make(chan struct{}, 1)
	go irqPeer()
}

func irqPeer() {
	for range irqKick {
		for irqWaiting.Load() {
			if !irqParked.Load() {
				var stats goroutine.Stats
				goroutine.ReadStats(&stats)
				irqParked.Store(stats.Waiters != 0)
			}
			irqSpins.Add(1)
			runtime.Gosched()
		}
	}
}

// irqWait counts a wait as proof when an interrupt ended it, the peer saw it
// registered, and the peer advanced during it.
func irqWait(irq handle.ID) kcall.Status {
	if irqProven == irqProofWaits {
		return sys.IRQWait(irq)
	}
	irqTries++
	irqParked.Store(false)
	before := irqSpins.Load()
	irqWaiting.Store(true)
	select {
	case irqKick <- struct{}{}:
	default:
	}
	status := sys.IRQWait(irq)
	irqWaiting.Store(false)
	spins := irqSpins.Load() - before
	if status != kcall.StatusOK || !irqParked.Load() || spins == 0 {
		return status
	}
	irqProven++
	irqCounted += spins
	if irqProven == irqProofWaits {
		close(irqKick)
		var line [96]byte
		text := append(line[:0], "VIRTIO_BLK_G_IRQ_OK waits="...)
		text = logfmt.AppendUint(text, irqProven)
		text = append(text, " tries="...)
		text = logfmt.AppendUint(text, irqTries)
		text = append(text, " peer="...)
		text = logfmt.AppendUint(text, irqCounted)
		mark(string(append(text, '\n')))
	}
	return status
}
