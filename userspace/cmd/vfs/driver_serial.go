//go:build goos_userspace && !goos_goroutines

package main

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/sys"
	userwait "goos/userspace/wait"
)

// Legacy builds retain the single-owner synchronous service loop.
const maxSessions = userwait.MaxObjects - 1

func initRuntime() { arena.InitService() }

func runService() {
	for {
		applyFilesystemOffline()
		var interests [userwait.MaxObjects]userwait.Interest
		var sessionIndexes [maxSessions]int
		interests[0] = userwait.Interest{Handle: controlHandle, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
		count := 1
		for index, session := range sessions {
			if session != 0 {
				interests[count] = userwait.Interest{Handle: handle.ID(session), Events: kcall.WaitReadable | kcall.WaitPeerClosed}
				sessionIndexes[count-1] = index
				count++
			}
		}
		waitInterests, waitCount = &interests, count
		if count-1 != liveSessions {
			write("VFS_SESSIONS_CORRUPT\n")
			if sys.DiagnosticsTrace(consoleHandle, sessions[:]) == kcall.StatusOK {
				writeSessions()
			}
			sys.Exit(exitFailed)
		}
		result, status := userwait.Any(&interests, count, kcall.WaitAnyInfiniteTimeout)
		if status != kcall.StatusOK {
			switch status {
			case kcall.StatusPermissionDenied:
				fatal("VFS_WAIT_PERMISSION_DENIED\n")
			case kcall.StatusNotFound:
				fatal("VFS_WAIT_NOT_FOUND\n")
			case kcall.StatusResourceExhausted:
				fatal("VFS_WAIT_RESOURCE_EXHAUSTED\n")
			case kcall.StatusPeerDied:
				fatal("VFS_WAIT_PEER_DIED\n")
			default:
				fatal("VFS_WAIT_FAILED\n")
			}
		}
		dispatchSerialBatch(result.ReadyMask, count, applyFilesystemOffline, func() {
			pollControl()
		}, func(interest int) {
			index := sessionIndexes[interest-1]
			session := sessions[index]
			message, receiveStatus := receiveNow(session)
			if receiveStatus == ipc.StatusPeerDied {
				// The files the session left open close with it, before its
				// slot, and with it its owner number, can be reused.
				closeSession(index)
				return
			}
			if receiveStatus != ipc.StatusOK {
				fatal("VFS_RECEIVE_FAILED\n")
			}
			serve(sessionOwner(index), session, message)
		})
	}
}
