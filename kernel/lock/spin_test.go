package lock

import (
	"goos/internal/validation"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

func TestSpinExcludesConcurrentHolders(t *testing.T) {
	if validation.Enabled {
		t.Skip("goroutines share one processor index, which validation builds treat as recursion")
	}
	Install(Hooks{Pause: runtime.Gosched})
	defer Install(Hooks{})
	var spin Spin
	var counter, inside int
	var group sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for round := 0; round < 2000; round++ {
				spin.Lock()
				inside++
				if inside != 1 {
					t.Error("two holders inside the critical section")
				}
				counter++
				inside--
				spin.Unlock()
			}
		}()
	}
	group.Wait()
	if counter != 8*2000 {
		t.Fatalf("counter = %d, want %d", counter, 8*2000)
	}
	if spin.Held() {
		t.Fatal("lock still held after every worker left")
	}
}

func TestTryLock(t *testing.T) {
	spin := New(LevelScheduler)
	if !spin.TryLock() {
		t.Fatal("free lock refused")
	}
	// A validation build treats the holder's second attempt as recursion.
	if !validation.Enabled && spin.TryLock() {
		t.Fatal("held lock taken twice")
	}
	spin.Unlock()
	if !spin.TryLock() {
		t.Fatal("released lock refused")
	}
	spin.Unlock()
}

func TestUnlockOfFreeLockIsFatal(t *testing.T) {
	var message string
	Install(Hooks{Fatal: func(text string) { message = text; panic(text) }})
	defer Install(Hooks{})
	defer func() {
		if recover() == nil || message == "" {
			t.Fatal("unlock of a free lock was accepted")
		}
	}()
	var spin Spin
	spin.Unlock()
}

func TestBitHelpers(t *testing.T) {
	var word atomic.Uint32
	setBit(&word, 1<<3)
	setBit(&word, 1<<5)
	clearBit(&word, 1<<3)
	if word.Load() != 1<<5 {
		t.Fatalf("word = %#x", word.Load())
	}
}

func TestLockUnlessGivesUpWhenStopped(t *testing.T) {
	Install(Hooks{})
	defer Install(Hooks{})
	holder := New(LevelProcess)
	holder.state.Store(1) // another processor holds it
	stop := uint64(1)
	waiter := &holder
	if waiter.LockUnless(&stop) {
		t.Fatal("a stopped waiter took a held lock")
	}
	holder.state.Store(0)
	stop = 0
	if !holder.LockUnless(&stop) {
		t.Fatal("a free lock refused")
	}
	holder.Unlock()
}

func TestContentionAndWatchdogPerLevel(t *testing.T) {
	if validation.Enabled {
		t.Skip("goroutines share one processor index, which validation builds treat as recursion")
	}
	var clock atomic.Uint64
	var stuck atomic.Uint32
	Install(Hooks{
		Pause:      func() { clock.Add(1); runtime.Gosched() },
		Now:        clock.Load,
		StuckAfter: 5000,
		Stuck:      func(level Level, holder uint32) { stuck.Store(uint32(level)) },
	})
	defer Install(Hooks{})
	before := Statistics(LevelStream)
	spin := New(LevelStream)
	spin.Lock()
	done := make(chan struct{})
	go func() {
		spin.Lock()
		spin.Unlock()
		close(done)
	}()
	for stuck.Load() == 0 {
		runtime.Gosched()
	}
	spin.Unlock()
	<-done
	after := Statistics(LevelStream)
	if Level(stuck.Load()) != LevelStream {
		t.Fatalf("watchdog reported level %d", stuck.Load())
	}
	if after.Contended != before.Contended+1 || after.Stuck != before.Stuck+1 || after.Longest < 5000 || after.Spin < after.Longest {
		t.Fatalf("stats %+v after %+v", after, before)
	}
	if LevelStream.String() != "stream" || Level(200).String() != "unknown" {
		t.Fatal("level names")
	}
}
