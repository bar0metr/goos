//go:build goos_userspace && goos_goroutines

package main

import (
	"goos/kernel/clock"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/vfs"
	"goos/userspace/runtime/sys"
	vfsservice "goos/userspace/vfs"
	userwait "goos/userspace/wait"
)

const (
	maxSessions = concurrentSessionLimit
	readWorkers = 2
)

type concurrentDriver struct {
	pool            *vfsservice.ReadPool
	requests        driverRequests
	completionRead  handle.ID
	controlPending  bool
	recoveryPending bool
	offlinePending  bool
	nextSession     int
}

var readDriver concurrentDriver

func runService() {
	pool, ok := vfsservice.NewReadPool(&service, readWorkers)
	if !ok {
		fatal("VFS_READ_POOL_FAILED\n")
	}
	reader, writer, status := sys.StreamCreate()
	if status != kcall.StatusOK {
		pool.Close()
		fatal("VFS_COMPLETION_STREAM_FAILED\n")
	}
	readDriver.pool, readDriver.completionRead = pool, reader
	go bridgeReadCompletions(pool.Wake(), writer)
	readDriver.run()
}

// bridgeReadCompletions converts a coalesced Go-channel wake into native
// readiness. Writes never block; a full stream is already readable. It uses
// no owner IPC wire, service state, validation hook, or logging scratch.
func bridgeReadCompletions(wake <-chan struct{}, writer handle.ID) {
	var token = [1]byte{1}
	for range wake {
		count, status := sys.StreamWrite(writer, token[:])
		if !completionWakeWritten(count, status) {
			sys.ConsoleWriteString(consoleHandle, "VFS_COMPLETION_WAKE_FAILED\n")
			sys.Exit(exitFailed)
		}
	}
}

func (driver *concurrentDriver) run() {
	for {
		driver.complete()
		if filesystemOfflinePending() && !driver.offlinePending && driver.pool.Available() != 0 {
			driver.admitBarrier(driverOffline, -1, ipc.Message{})
			driver.offlinePending = true
			continue
		}
		var interests [userwait.MaxObjects]userwait.Interest
		var indexes [userwait.MaxObjects]int
		interests[0] = userwait.Interest{Handle: driver.completionRead, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
		count, controlIndex := 1, -1
		if driver.pool.Available() != 0 && !driver.controlPending && !driver.recoveryPending {
			controlIndex = count
			interests[count] = userwait.Interest{Handle: controlHandle, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
			count++
		}
		recoveryIndex := -1
		if driver.pool.Available() != 0 && !driver.recoveryPending {
			recoveryIndex = count
			interests[count] = userwait.Interest{Handle: recoveryHandle, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
			count++
		}
		firstSession := count
		observed := 0
		for step := 0; step < maxSessions; step++ {
			index := (driver.nextSession + step) % maxSessions
			session := sessions[index]
			if session == 0 {
				continue
			}
			observed++
			if driver.pool.Available() == 0 || driver.pool.Busy(sessionOwner(index)) {
				continue
			}
			interests[count] = userwait.Interest{Handle: handle.ID(session), Events: kcall.WaitReadable | kcall.WaitPeerClosed}
			indexes[count] = index
			count++
		}
		waitInterests, waitCount = &interests, count
		if observed != liveSessions {
			fatal("VFS_SESSIONS_CORRUPT\n")
		}
		timeout := uint64(kcall.WaitAnyInfiniteTimeout)
		if readPoolProbePending() {
			timeout = clock.TicksPerSecond / 20
		}
		ready, status := userwait.Any(&interests, count, timeout)
		if status == kcall.StatusTimeout && readPoolProbePending() {
			readPoolProbePoll()
			continue
		}
		if status != kcall.StatusOK {
			fatal("VFS_WAIT_FAILED\n")
		}
		if ready.ReadyMask&1 != 0 {
			driver.drainWake()
		}
		if controlIndex >= 0 && ready.ReadyMask&(uint64(1)<<controlIndex) != 0 && driver.pool.Available() != 0 {
			message, status := receiveNow(uint64(controlHandle))
			if status == ipc.StatusOK {
				driver.admitBarrier(driverControl, -1, message)
				driver.controlPending = true
			} else if status != ipc.StatusNotFound {
				fatal("VFS_CONTROL_FAILED\n")
			}
		}
		if recoveryIndex >= 0 && ready.ReadyMask&(uint64(1)<<recoveryIndex) != 0 && driver.pool.Available() != 0 {
			message, status := receiveNow(uint64(recoveryHandle))
			if status == ipc.StatusOK {
				driver.admitBarrier(driverRecovery, -1, message)
				driver.recoveryPending = true
			} else if status != ipc.StatusNotFound {
				fatal("VFS_RECOVERY_FAILED\n")
			}
		}
		for interest := firstSession; interest < count && driver.pool.Available() != 0; interest++ {
			if ready.ReadyMask&(uint64(1)<<interest) == 0 {
				continue
			}
			index := indexes[interest]
			message, status := receiveNow(sessions[index])
			switch status {
			case ipc.StatusPeerDied:
				driver.admitBarrier(driverClose, index, ipc.Message{})
			case ipc.StatusOK:
				driver.admitSession(index, message)
			case ipc.StatusNotFound:
				continue
			default:
				fatal("VFS_RECEIVE_FAILED\n")
			}
			driver.nextSession = (index + 1) % maxSessions
		}
	}
}

func (driver *concurrentDriver) drainWake() {
	var data [64]byte
	for {
		count, status := sys.StreamRead(driver.completionRead, data[:])
		if status == kcall.StatusResourceExhausted && count == 0 {
			return
		}
		if status != kcall.StatusOK || count == 0 {
			fatal("VFS_COMPLETION_STREAM_FAILED\n")
		}
	}
}

func (driver *concurrentDriver) remember(request driverRequest) {
	if !driver.requests.put(request) {
		fatal("VFS_REQUEST_TABLE_FAILED\n")
	}
}

func (driver *concurrentDriver) admitBarrier(kind driverRequestKind, index int, message ipc.Message) {
	request := driverRequest{kind: kind, index: index, message: message}
	session := vfsservice.ReadSession{}
	if index >= 0 {
		request.session, request.generation = sessions[index], sessionGenerations[index]
		session = vfsservice.ReadSession{Owner: sessionOwner(index), Generation: request.generation}
	}
	correlation := message.Correlation
	if correlation == 0 {
		// Pool ordering also covers malformed requests and synthetic close/
		// offline events. Their original wire correlation stays in message.
		correlation = 1
	}
	ticket, status := driver.pool.AdmitBarrier(session, correlation)
	if status != vfs.StatusOK {
		fatal("VFS_BARRIER_ADMISSION_FAILED\n")
	}
	request.ticket = ticket
	driver.remember(request)
	readPoolProbeBarrierAdmitted(kind, session.Owner, message)
}

func (driver *concurrentDriver) admitSession(index int, message ipc.Message) {
	request, parallel := parallelRead(message)
	var base, size uint64
	var target []byte
	if parallel && request.Operation == vfs.OperationReadInto {
		var status kcall.Status
		base, size, status = sys.MemoryMapAnywhere(message.Handles[0], true)
		if status != kcall.StatusOK {
			parallel = false
		} else if end, valid := bulkReadWindow(size, request.Offset, request.Size); valid {
			target = sys.Window(base, size)[request.Offset:end]
			readPoolProbeMapped(sessionOwner(index), request.Handle)
		} else {
			sys.VMUnmap(base, size)
			base, size, parallel = 0, 0, false
		}
	}
	if !parallel {
		// Original serve handles invalid forms and attachment/map failures,
		// unchanged, after the serial barrier has drained prior readers.
		driver.admitBarrier(driverServe, index, message)
		return
	}
	beginServing(sessionOwner(index), sessions[index])
	session := vfsservice.ReadSession{Owner: sessionOwner(index), Generation: sessionGenerations[index]}
	var ticket uint64
	var status vfs.Status
	readPoolProbeReadAdmitted(session.Owner, request.Handle)
	if request.Operation == vfs.OperationReadInto {
		ticket, status = driver.pool.AdmitReadInto(session, message.Correlation, request.Handle, target)
	} else {
		ticket, status = driver.pool.AdmitRead(session, message.Correlation, request.Handle, request.Size)
	}
	if status != vfs.StatusOK {
		if size != 0 {
			sys.VMUnmap(base, size)
		}
		closeAttached(message.Handles[:message.HandleCount])
		fatal("VFS_READ_ADMISSION_FAILED\n")
	}
	driver.remember(driverRequest{
		ticket: ticket, kind: driverRead, index: index,
		session: sessions[index], generation: session.Generation,
		message: message, operation: request.Operation, file: request.Handle, mapBase: base, mapSize: size,
	})
}

func (driver *concurrentDriver) complete() {
	for {
		completion, ok := driver.pool.Poll()
		if !ok {
			return
		}
		request := driver.requests.find(completion.Ticket)
		if request == nil {
			fatal("VFS_COMPLETION_TICKET_FAILED\n")
		}
		current := request.index >= 0 && sameSession(request.session, request.generation, sessions[request.index], sessionGenerations[request.index])
		if completion.Barrier {
			applyFilesystemOffline()
		}
		switch request.kind {
		case driverRead:
			if current {
				answer := vfs.Reply{Operation: request.operation, Status: completion.Status, Size: uint64(completion.Count)}
				if request.operation == vfs.OperationRead {
					answer.Data = completion.Data
				}
				reply, valid := vfs.EncodeReply(answer, request.message.Correlation)
				if !valid {
					fatal("VFS_REPLY_FAILED\n")
				}
				send(request.session, reply)
			}
			if request.mapSize != 0 {
				sys.VMUnmap(request.mapBase, request.mapSize)
				readPoolProbeUnmapped(sessionOwner(request.index), request.file)
			}
			closeAttached(request.message.Handles[:request.message.HandleCount])
			readPoolProbeReadCompleted()
			readPoolProbeLocalCompleted()
			readPoolProbePersistentCompleted()
		case driverServe:
			if current {
				serve(sessionOwner(request.index), request.session, request.message)
				readPoolProbeBarrierCompleted(driverServe, sessionOwner(request.index), request.message)
			} else {
				closeAttached(request.message.Handles[:request.message.HandleCount])
			}
		case driverControl:
			processControl(request.message)
			driver.controlPending = false
			readPoolProbeBarrierCompleted(driverControl, 0, request.message)
		case driverClose:
			if current {
				closeSession(request.index)
			}
		case driverOffline:
			driver.offlinePending = false
		case driverRecovery:
			processRecovery(request.message)
			driver.recoveryPending = false
		}
		if !driver.requests.release(completion.Ticket) || !driver.pool.Retire(completion.Ticket) {
			fatal("VFS_RETIRE_FAILED\n")
		}
	}
}
