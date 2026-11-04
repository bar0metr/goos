//go:build uefi

package main

import (
	"goos/internal/capability"
	linuxcompat "goos/kernel/compat/linux"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/process"
)

// LinuxNetAttach gives a Linux family a new GLNXSOCK2 session (ADR 0210).
// The caller names the family by a process handle with Write to its launch
// root, the one PID 1 holds from the launch, and hands over a channel with
// Read, Write, and Wait and a pool of linuxsockapi.PoolPages pages with
// Read and Write, both with Transfer. They move into the root's handle
// table. A session that still works is not replaced: AlreadyExists. One
// that failed, or whose service is gone, is: its handles close and every
// socket of it stays failed.
func (handler nativeCallHandler) LinuxNetAttach(processHandle uint64, channelHandle uint64, poolHandle uint64) kcall.Status {
	owner, found := handler.self()
	if !found {
		return kcall.StatusPermissionDenied
	}
	authority, allowed := owner.Handles().Lookup(handle.ID(processHandle), handle.TypeProcess, capability.Write)
	if !allowed {
		return kcall.StatusPermissionDenied
	}
	target, exists := processRegistry.ByID(process.ID(authority.Object()))
	if !exists || target == owner || target.ABI() != process.ABILinuxX8664 {
		return kcall.StatusNotFound
	}
	state, linux := linuxcompat.FindProcess(target.AddressSpace().Root)
	if !linux || state.Forked {
		return kcall.StatusInvalidArgument
	}
	familyID := state.FamilyID
	_, channelOK := owner.Handles().Lookup(handle.ID(channelHandle), handle.TypeChannel, capability.Read|capability.Write|capability.Wait|capability.Transfer)
	_, poolOK := owner.Handles().Lookup(handle.ID(poolHandle), handle.TypeMemory, capability.Read|capability.Write|capability.Transfer)
	if !channelOK || !poolOK {
		return kcall.StatusPermissionDenied
	}
	if _, valid := resolveLinuxNet(owner, handle.ID(channelHandle), handle.ID(poolHandle)); !valid {
		return kcall.StatusInvalidArgument
	}
	if endpoint, live, _ := linuxcompat.NetSession(familyID); live && channelSideOpen(endpoint.Channel, endpoint.Side) && channelSideOpen(endpoint.Channel, endpoint.Side^1) {
		return kcall.StatusAlreadyExists
	}
	// The handles move into the root's table, which the call must hold. It
	// has changed nothing yet; the dispatcher runs it again holding it.
	if !wantTransferLocks(target, false) {
		return kcall.StatusOK
	}
	if target.Handles().Available() < 2 {
		return kcall.StatusResourceExhausted
	}
	movedChannel, channelMoved := owner.Handles().Transfer(target.Handles(), handle.ID(channelHandle), handle.TypeChannel, capability.Read|capability.Write|capability.Wait)
	if !channelMoved {
		return kcall.StatusResourceExhausted
	}
	movedPool, poolMoved := owner.Handles().Transfer(target.Handles(), handle.ID(poolHandle), handle.TypeMemory, capability.Read|capability.Write)
	if !poolMoved {
		target.Handles().Close(movedChannel)
		return kcall.StatusResourceExhausted
	}
	closeMovedHandle(owner.Handles(), handle.ID(channelHandle))
	closeMovedHandle(owner.Handles(), handle.ID(poolHandle))
	grant, valid := resolveLinuxNet(target, movedChannel, movedPool)
	if !valid {
		closeLinuxNetHandles(target, linuxcompat.NetGrant{ChannelHandle: movedChannel, PoolHandle: movedPool})
		return kcall.StatusInvalidArgument
	}
	old, replaced, attached := linuxcompat.AttachNet(familyID, grant, true)
	if !attached {
		closeLinuxNetHandles(target, grant)
		return kcall.StatusInvalidArgument
	}
	if replaced {
		if holder, live := processRegistry.ByID(old.Holder); live && holder == target {
			closeLinuxNetHandles(target, old)
		} else {
			// The old holder is gone and took its handles with it; its
			// waiters still learn that the session ended.
			if closeChannelSide(old.Channel, old.Side) {
				wakeIPCWaiters(old.Channel, old.Side^1, ipc.WaitPeerDied)
			}
			wakeIPCWaiters(old.Channel, old.Side, ipc.WaitPeerDied)
		}
	}
	return kcall.StatusOK
}
