//go:build goos_userspace && goos_goroutines

package main

import (
	"sync"

	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/ramfs"
	"goos/userspace/fsproto"
	syslog "goos/userspace/log"
	"goos/userspace/runtime/sys"
	userwait "goos/userspace/wait"
)

// readPool runs the read-only requests on bounded worker goroutines. All its
// methods except the worker loop belong to the owner goroutine.
type readPool struct {
	workers    int
	jobs       chan readJob
	results    chan readJob
	wakeWriter handle.ID
	wakeToken  [1]byte
	joined     sync.WaitGroup
	inFlight   int
}

func newReadPool(workers int, wakeWriter handle.ID) (*readPool, bool) {
	if workers < 1 || workers > maxReadWorkers {
		return nil, false
	}
	pool := &readPool{
		workers:    workers,
		jobs:       make(chan readJob, workers),
		results:    make(chan readJob, workers),
		wakeWriter: wakeWriter,
	}
	pool.wakeToken[0] = 1
	pool.joined.Add(workers)
	for index := 0; index < workers; index++ {
		go pool.worker()
	}
	return pool, true
}

func (pool *readPool) worker() {
	defer pool.joined.Done()
	var name [ramfs.MaxName]byte
	for job := range pool.jobs {
		if restartGateRead(job.request) && holdRestartRead() {
			job.reply = fsproto.Reply{Operation: job.request.Operation, Status: ramfs.StatusIOError}
		} else {
			job.reply = answer(job.request, &name)
		}
		pool.results <- job
		// Wake the owner straight from the worker, with one stream write;
		// a full stream is already readable, so a dropped wake is fine.
		sys.StreamWrite(pool.wakeWriter, pool.wakeToken[:])
	}
}

// admit queues a read for a worker; the caller has already ensured a slot is
// free, so the buffered send cannot block.
func (pool *readPool) admit(request fsproto.Request, correlation uint64) {
	// adoptReadJob owns the name before handoff: request.Name aliases
	// owner-owned decode storage that the next receive overwrites.
	job := adoptReadJob(request, correlation)
	pool.inFlight++
	pool.jobs <- job
}

// collect replies to every finished read without blocking.
func (pool *readPool) collect() {
	for {
		select {
		case job := <-pool.results:
			pool.inFlight--
			sendReply(job.reply, job.correlation)
		default:
			return
		}
	}
}

// drainAndReply waits for every in-flight read to finish and replies to it,
// so a mutation or a commit runs after all readers have retired.
func (pool *readPool) drainAndReply() {
	for pool.inFlight > 0 {
		job := <-pool.results
		pool.inFlight--
		sendReply(job.reply, job.correlation)
	}
}

// readOnly reports whether the operation may run on a worker.
func readOnly(operation fsproto.Operation) bool {
	switch operation {
	case fsproto.OperationStat, fsproto.OperationOpen, fsproto.OperationRead, fsproto.OperationFilesystemInfo:
		return true
	}
	return false
}

// bridgeReadCompletions is no longer used: workers write the wake stream
// directly, so there is no extra goroutine hop between a finished read and
// the owner's wait.

func runService() {
	reader, writer, status := sys.StreamCreate()
	if status != kcall.StatusOK {
		fatal("EXT4_COMPLETION_STREAM_FAILED\n")
	}
	pool, ok := newReadPool(maxReadWorkers, writer)
	if !ok {
		fatal("EXT4_READ_POOL_FAILED\n")
	}
	runOwner(pool, reader)
}

// runOwner receives requests, admits read-only work to the pool, and runs
// mutations (and the owner-serialized reads) inline after draining readers.
func runOwner(pool *readPool, completionRead handle.ID) {
	commitDue := uint64(0)
	for {
		pool.collect()
		wait := uint64(requestWait)
		if volume.Pending() {
			now := ticks()
			if commitDue == 0 {
				commitDue = now + commitInterval
			}
			if now >= commitDue {
				commitDue = 0
				pool.drainAndReply()
				volume.Flush()
				exitIfFailed()
				continue
			}
			wait = commitDue - now
		} else {
			commitDue = 0
		}
		var interests [userwait.MaxObjects]userwait.Interest
		interests[0] = userwait.Interest{Handle: completionRead, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
		interests[1] = userwait.Interest{Handle: blockHandle, Events: kcall.WaitPeerClosed}
		count := 2
		// Stop receiving when the pool is full, so an admitted request is
		// never dropped; a completion wakes the loop and frees a slot.
		if pool.inFlight < pool.workers {
			interests[count] = userwait.Interest{Handle: vfsHandle, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
			count++
		}
		ready, waited := userwait.Any(&interests, count, wait)
		if waited == kcall.StatusTimeout {
			continue
		}
		if waited != kcall.StatusOK {
			fatal("EXT4_WAIT_FAILED\n")
		}
		if ready.ReadyMask&1 != 0 {
			drainWake(completionRead)
			continue
		}
		if ready.ReadyMask&2 != 0 {
			pool.drainAndReply()
			logCounters()
			sys.Exit(exitBlockGone)
		}
		status := sys.IPCReceive(vfsHandle, wire[:])
		if status == kcall.StatusNotFound {
			continue
		}
		if status == kcall.StatusPeerDied {
			pool.drainAndReply()
			volume.Flush()
			syslog.Warn("vfs endpoint closed")
			logCounters()
			sys.Exit(exitVFSGone)
		}
		if status != kcall.StatusOK {
			fatal("EXT4_RECEIVE_FAILED\n")
		}
		if !ipc.DecodeInto(wire[:wireSize(wire[:])], &requestMessage) {
			continue
		}
		request, valid := fsproto.DecodeRequest(&requestMessage, cache, &requestedName)
		var reply fsproto.Reply
		switch {
		case !valid:
			operation := fsproto.Operation(requestMessage.Payload[0])
			if requestMessage.Correlation == 0 || operation < fsproto.OperationMount || operation > fsproto.OperationUnmount {
				continue
			}
			reply = fsproto.Reply{Operation: operation, Status: ramfs.StatusInvalidArgument}
		case readOnly(request.Operation):
			// Commit pending mutations before a reader sees them, so a
			// read-after-write observes the write (ADR 0232).
			if volume.Pending() {
				pool.drainAndReply()
				volume.Flush()
				exitIfFailed()
			}
			// Serve inline when nothing else is queued or in flight: the
			// pool's hand-off costs more than the request for a lone client,
			// and a serial read is already correct.
			if pool.inFlight == 0 && !restartGateRead(request) && !requestPending() {
				reply = answer(request, &listedName)
				sendReply(reply, requestMessage.Correlation)
				exitIfFailed()
				continue
			}
			pool.admit(request, requestMessage.Correlation)
			continue
		default:
			pool.drainAndReply()
			reply = answer(request, &listedName)
		}
		sendReply(reply, requestMessage.Correlation)
		exitIfFailed()
	}
}

// requestPending reports whether another request is already queued on the VFS
// endpoint, without waiting.
func requestPending() bool {
	var interests [userwait.MaxObjects]userwait.Interest
	interests[0] = userwait.Interest{Handle: vfsHandle, Events: kcall.WaitReadable}
	ready, status := userwait.Any(&interests, 1, 0)
	return status == kcall.StatusOK && ready.ReadyMask&1 != 0
}

func drainWake(completionRead handle.ID) {
	var data [64]byte
	for {
		count, status := sys.StreamRead(completionRead, data[:])
		if status == kcall.StatusResourceExhausted && count == 0 {
			return
		}
		if status != kcall.StatusOK || count == 0 {
			fatal("EXT4_COMPLETION_STREAM_FAILED\n")
		}
	}
}
