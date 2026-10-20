//go:build !goos_userspace || goos_goroutines

package vfsservice

import (
	"goos/kernel/vfs"
	"sync"
)

const (
	// ReadQueueCapacity bounds every admitted request, including queued work,
	// completed replies not yet retired, and serial barriers.
	ReadQueueCapacity = 8
	MaxReadWorkers    = 4
)

// ReadSession identifies a session lifetime, not only its reusable owner slot.
// A driver must drain and retire its requests before closing the session.
type ReadSession struct {
	Owner      vfs.Owner
	Generation uint64
}

// ReadCompletion borrows Data until Retire. The owner encodes/sends the reply
// (or executes a Barrier) before retiring it. Workers never send client IPC.
type ReadCompletion struct {
	Ticket      uint64
	Session     ReadSession
	Correlation uint64
	Barrier     bool
	Count       int
	Status      vfs.Status
	Data        []byte
}

type readState uint8

const (
	readFree readState = iota
	readQueued
	readRunning
	readCompleted
	readDelivered
)

type queuedRead struct {
	completion ReadCompletion
	file       vfs.Handle
	plan       ReadPlan
	planned    bool
	state      readState
	target     []byte
	inline     [vfs.MaxReplyData]byte
}

type readWork struct {
	slot   int
	ticket uint64
	plan   ReadPlan
	target []byte
}

type readResult struct {
	slot   int
	ticket uint64
	count  int
	status vfs.Status
}

// ReadPool runs backend reads with compiler-supported goroutines and channels.
// Production builds select goos_goroutines; the legacy native-thread target
// cannot compile this pool. All methods except Wake belong to one owner, which
// also owns Service, admission order, plan/commit, and client reply lifetimes.
// Workers own only the immutable work value and its target until completion.
// No service-wide lock is held during backend execution.
//
// Only reads in the prefix before the next barrier may run. Persistent reads
// serialize the whole Remote.Read; independent local reads can pass a queued
// persistent read in that prefix. Unknown backends are serial barriers too.
// The caller must route every other service/control operation through a
// barrier; calling Dispatch while this pool owns reads is not supported.
type ReadPool struct {
	service *Service
	workers int
	jobs    chan readWork
	results chan readResult
	wake    chan struct{}
	joined  sync.WaitGroup

	slots   [ReadQueueCapacity]queuedRead
	order   [ReadQueueCapacity]int
	count   int
	running int
	next    uint64
	closed  bool
}

// NewReadPool creates a fixed worker set. Construction is separate from the
// driver so host tests exercise the same channel-based pool as production.
func NewReadPool(service *Service, workers int) (*ReadPool, bool) {
	if service == nil || workers < 1 || workers > MaxReadWorkers {
		return nil, false
	}
	pool := &ReadPool{
		service: service,
		workers: workers,
		jobs:    make(chan readWork, workers),
		results: make(chan readResult, workers),
		wake:    make(chan struct{}, 1),
	}
	pool.joined.Add(workers)
	for index := 0; index < workers; index++ {
		go pool.worker()
	}
	return pool, true
}

func (pool *ReadPool) worker() {
	defer pool.joined.Done()
	for work := range pool.jobs {
		count, status := work.plan.Read(work.target)
		pool.results <- readResult{slot: work.slot, ticket: work.ticket, count: count, status: status}
		select {
		case pool.wake <- struct{}{}:
		default:
		}
	}
}

// Wake is a coalesced hint to call Poll, not one notification per result. A
// driver must drain Poll before waiting again. The channel is never closed.
func (pool *ReadPool) Wake() <-chan struct{} { return pool.wake }

// Available is the number of admission slots left, including the slots
// barriers need. A driver uses it to stop receiving before ingress is full.
func (pool *ReadPool) Available() int {
	if pool.closed {
		return 0
	}
	return ReadQueueCapacity - pool.count
}

// Busy reports whether an owner still has an admitted request. A session's
// readable interest must stay disabled until its previous reply is retired.
func (pool *ReadPool) Busy(owner vfs.Owner) bool {
	if owner == 0 {
		return false
	}
	for index := 0; index < pool.count; index++ {
		if pool.slots[pool.order[index]].completion.Session.Owner == owner {
			return true
		}
	}
	return false
}

// AdmitRead admits an inline read. Its target belongs to the bounded slot,
// not the Service's shared Dispatch buffer. Size is clamped as Dispatch does.
func (pool *ReadPool) AdmitRead(session ReadSession, correlation uint64, file vfs.Handle, size uint32) (uint64, vfs.Status) {
	slot, status := pool.admit(session, correlation, false)
	if status != vfs.StatusOK {
		return 0, status
	}
	entry := &pool.slots[slot]
	entry.file = file
	entry.target = entry.inline[:min(uint64(size), uint64(len(entry.inline)))]
	pool.schedule()
	return entry.completion.Ticket, vfs.StatusOK
}

// AdmitReadInto borrows a caller-validated bulk mapping until Retire. The
// driver retains its attached handle and mapping until then; admission does
// not map memory or transfer ownership of cleanup to the worker.
func (pool *ReadPool) AdmitReadInto(session ReadSession, correlation uint64, file vfs.Handle, target []byte) (uint64, vfs.Status) {
	slot, status := pool.admit(session, correlation, false)
	if status != vfs.StatusOK {
		return 0, status
	}
	entry := &pool.slots[slot]
	entry.file, entry.target = file, target
	pool.schedule()
	return entry.completion.Ticket, vfs.StatusOK
}

// AdmitBarrier orders a mutation/control/session-close operation. An empty
// session names a global control. The owner performs the action only when
// Poll delivers this barrier and retires it only after its reply/cleanup.
func (pool *ReadPool) AdmitBarrier(session ReadSession, correlation uint64) (uint64, vfs.Status) {
	slot, status := pool.admit(session, correlation, true)
	if status != vfs.StatusOK {
		return 0, status
	}
	pool.schedule()
	return pool.slots[slot].completion.Ticket, vfs.StatusOK
}

func (pool *ReadPool) admit(session ReadSession, correlation uint64, barrier bool) (int, vfs.Status) {
	if pool.closed || correlation == 0 || session.Owner == 0 && (!barrier || session.Generation != 0) || session.Owner != 0 && session.Generation == 0 {
		return 0, vfs.StatusInvalidArgument
	}
	if pool.Busy(session.Owner) {
		return 0, vfs.StatusBusy
	}
	if pool.count == ReadQueueCapacity || pool.next == ^uint64(0) {
		return 0, vfs.StatusResourceExhausted
	}
	for slot := range pool.slots {
		if pool.slots[slot].state != readFree {
			continue
		}
		pool.next++
		pool.slots[slot] = queuedRead{
			state: readQueued,
			completion: ReadCompletion{
				Ticket: pool.next, Session: session, Correlation: correlation, Barrier: barrier,
			},
		}
		pool.order[pool.count] = slot
		pool.count++
		return slot, vfs.StatusOK
	}
	return 0, vfs.StatusResourceExhausted
}

func (pool *ReadPool) schedule() {
	persistent := false
	for index := 0; index < pool.count; index++ {
		entry := &pool.slots[pool.order[index]]
		if entry.state == readRunning && entry.plan.Lane() == ReadLanePersistent {
			persistent = true
		}
	}
	for index := 0; index < pool.count; index++ {
		slot := pool.order[index]
		entry := &pool.slots[slot]
		if entry.completion.Barrier {
			if index == 0 && entry.state == readQueued {
				entry.state = readCompleted
			}
			break
		}
		if entry.planned && entry.plan.Lane() == ReadLaneSerial && entry.state != readQueued {
			break
		}
		if entry.state != readQueued {
			continue
		}
		if pool.running == pool.workers {
			break
		}
		if !entry.planned {
			plan, status := pool.service.BeginRead(entry.completion.Session.Owner, entry.file)
			if status != vfs.StatusOK {
				entry.completion.Status, entry.state = status, readCompleted
				continue
			}
			entry.plan, entry.planned = plan, true
		}
		switch entry.plan.Lane() {
		case ReadLaneSerial:
			if index != 0 {
				return
			}
		case ReadLanePersistent:
			if persistent {
				continue
			}
			persistent = true
		}
		entry.state = readRunning
		pool.running++
		// At most workers jobs, including completed results not collected yet,
		// are outstanding, so this bounded send cannot wait for a worker.
		pool.jobs <- readWork{slot: slot, ticket: entry.completion.Ticket, plan: entry.plan, target: entry.target}
		if entry.plan.Lane() == ReadLaneSerial {
			break
		}
	}
}

// Poll collects worker results, commits their cursors on the owner, and
// delivers at most one completion. A delivered slot stays occupied until
// Retire, even though its backend execution has already ended.
func (pool *ReadPool) Poll() (ReadCompletion, bool) {
	for {
		select {
		case result := <-pool.results:
			entry := &pool.slots[result.slot]
			if entry.state != readRunning || entry.completion.Ticket != result.ticket {
				panic("vfs read pool: stale worker result")
			}
			pool.running--
			entry.completion.Count = result.count
			entry.completion.Status = pool.service.FinishRead(entry.plan, result.count, result.status)
			entry.completion.Data = entry.target[:result.count]
			entry.state = readCompleted
		default:
			pool.schedule()
			for index := 0; index < pool.count; index++ {
				entry := &pool.slots[pool.order[index]]
				if entry.state == readCompleted {
					entry.state = readDelivered
					return entry.completion, true
				}
			}
			return ReadCompletion{}, false
		}
	}
}

// Retire releases a delivered request after reply delivery or abandonment.
// Owner slots, inline scratch and barriers cannot be reused before this call.
func (pool *ReadPool) Retire(ticket uint64) bool {
	for index := 0; index < pool.count; index++ {
		slot := pool.order[index]
		entry := &pool.slots[slot]
		if entry.completion.Ticket != ticket || entry.state != readDelivered {
			continue
		}
		*entry = queuedRead{}
		copy(pool.order[index:], pool.order[index+1:pool.count])
		pool.count--
		pool.order[pool.count] = 0
		pool.schedule()
		return true
	}
	return false
}

// Close joins all workers only when every admitted request has been retired.
// A busy pool is unchanged; the owner must keep draining its completions.
func (pool *ReadPool) Close() bool {
	if pool.count != 0 {
		return false
	}
	if !pool.closed {
		pool.closed = true
		close(pool.jobs)
		pool.joined.Wait()
	}
	return true
}
