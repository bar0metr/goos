//go:build uefi

package main

import (
	"goos/internal/capability"
	"goos/kernel/arch/amd64"
	"goos/kernel/clock"
	"goos/kernel/diag"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/process"
	"goos/kernel/sched"
	"goos/kernel/stream"
	kernelwait "goos/kernel/wait"
)

var diagRing diag.Ring

func trace(kind diag.Kind, pid process.ID, task uint64, object uint64, value uint64) {
	if !diag.Enabled {
		return
	}
	diagRing.Record(diag.Event{Tick: amd64.TimerTicks(), Kind: kind, Process: uint64(pid), Task: task, Object: object, Value: value})
}

func traceWaitKey(kind diag.Kind, pid process.ID, task sched.TaskID, key kernelwait.Key, value uint64) {
	trace(kind, pid, uint64(task), key.Object|uint64(key.Side)<<63, value)
}

func pidOfTask(task sched.TaskID) process.ID {
	for index := 0; ; index++ {
		owner, found := processRegistry.At(index)
		if !found {
			return 0
		}
		if owner.OwnsTask(task) {
			return owner.ID()
		}
	}
}

// Diagnostics prints a kernel state dump on the serial console in goos_diag
// builds. The caller needs the same console authority it would need to print
// the dump itself.
func (handler nativeCallHandler) Diagnostics(device uint64) kcall.Status {
	if !diag.Enabled {
		return kcall.StatusUnsupported
	}
	owner, found := processRegistry.ByRoot(handler.root)
	if !found {
		return kcall.StatusPermissionDenied
	}
	authority, allowed := owner.Handles().Lookup(handle.ID(device), handle.TypeDevice, capability.Write)
	if !allowed || authority.Object() != 1 {
		return kcall.StatusPermissionDenied
	}
	dumpDiagnostics(owner.ID())
	return kcall.StatusOK
}

type diagWriter struct{ console amd64.COM1 }

func (writer diagWriter) text(value string) { writer.console.WriteString(value) }

func (writer diagWriter) hex(name string, value uint64) {
	writer.console.WriteString(" ")
	writer.console.WriteString(name)
	writer.console.WriteString("=")
	writer.console.WriteHex(value)
}

func (writer diagWriter) uint(name string, value uint64) {
	writer.console.WriteString(" ")
	writer.console.WriteString(name)
	writer.console.WriteString("=")
	writer.console.WriteUint(value)
}

func (writer diagWriter) word(name, value string) {
	writer.console.WriteString(" ")
	writer.console.WriteString(name)
	writer.console.WriteString("=")
	writer.console.WriteString(value)
}

func (writer diagWriter) end() { writer.console.WriteString("\n") }

var handleTypeNames = [...]string{"invalid", "process", "task", "memory", "channel", "device", "irq", "stream"}

func dumpDiagnostics(requester process.ID) {
	out := diagWriter{console: amd64.NewCOM1()}
	state := amd64.ReadSchedulerState()
	out.text("DIAG begin")
	out.uint("tick", amd64.TimerTicks())
	out.uint("hz", clock.TicksPerSecond)
	out.uint("requester", uint64(requester))
	out.end()
	out.text("DIAG sched")
	out.hex("runnable", state.Runnable)
	out.hex("dead", state.Dead)
	out.uint("slots", state.Count)
	out.uint("current", state.Current)
	out.end()
	for index := 0; ; index++ {
		owner, found := processRegistry.At(index)
		if !found {
			break
		}
		dumpProcess(out, owner, state)
	}
	for index := 0; index < ipc.MaxChannels; index++ {
		id, channel, found := channelRegistry.At(index)
		if !found {
			continue
		}
		out.text("DIAG channel")
		out.hex("id", id)
		out.uint("alive0", boolValue(channel.Alive(0)))
		out.uint("alive1", boolValue(channel.Alive(1)))
		out.uint("queued0", uint64(channel.Queued(0)))
		out.uint("queued1", uint64(channel.Queued(1)))
		out.uint("pending0", uint64(channel.Pending(0)))
		out.uint("pending1", uint64(channel.Pending(1)))
		for side := uint8(0); side <= 1; side++ {
			object, _ := ipc.EndpointObject(id, side)
			holder, held := processRegistry.ByCapabilityObject(handle.TypeChannel, object)
			if held {
				out.uint(sideName(side), uint64(holder.ID()))
			}
		}
		out.end()
	}
	for slot := 0; slot < sched.MaxTasks; slot++ {
		view, found := waitAnyTable.At(slot)
		if !found {
			continue
		}
		out.text("DIAG waitany")
		out.hex("task", uint64(view.Task))
		out.uint("pid", uint64(pidOfTask(view.Task)))
		switch {
		case view.TimedOut:
			out.word("state", "timed-out")
		case view.Complete:
			out.word("state", "complete")
		default:
			out.word("state", "active")
		}
		out.uint("deadline", view.Deadline)
		out.end()
		for entry := 0; entry < view.Count; entry++ {
			out.text("DIAG   interest")
			out.uint("kind", uint64(view.Entries[entry].Key.Kind))
			out.hex("object", view.Entries[entry].Key.Object)
			out.uint("side", uint64(view.Entries[entry].Key.Side))
			out.hex("events", uint64(view.Entries[entry].Events))
			out.hex("ready", uint64(view.Ready[entry]))
			out.end()
		}
	}
	out.text("DIAG waits")
	out.uint("ipc", uint64(ipcWaitSet.Count()))
	out.uint("stream", uint64(streamWaitSet.Count()))
	out.uint("process", uint64(processWaitSet.Count()))
	out.uint("waitany", uint64(waitAnyTable.Count()))
	out.uint("lifecycle-pending", uint64(pendingLifecycleCount))
	out.uint("lifecycle-overflows", lifecycleOverflows)
	out.end()
	dumpTiming(out)
	out.text("DIAG events")
	out.uint("total", diagRing.Total())
	out.end()
	for index := 0; index < diagRing.Len(); index++ {
		event := diagRing.At(index)
		out.text("DIAG event")
		out.uint("tick", event.Tick)
		out.word("kind", event.Kind.String())
		out.uint("pid", event.Process)
		out.hex("task", event.Task)
		out.hex("object", event.Object)
		if event.Kind == diag.WaitDenied {
			out.word("reason", diag.Deny(event.Value).String())
		} else {
			out.hex("value", event.Value)
		}
		out.end()
	}
	out.text("DIAG end\n")
}

func dumpProcess(out diagWriter, owner *process.Process, state amd64.SchedulerState) {
	out.text("DIAG process")
	out.uint("pid", uint64(owner.ID()))
	out.hex("root", owner.AddressSpace().Root)
	for _, task := range owner.Tasks() {
		slot, _, valid := task.Parts()
		taskState := "invalid"
		if valid {
			switch {
			case state.Dead&(1<<slot) != 0:
				taskState = "dead"
			case state.Runnable&(1<<slot) != 0:
				taskState = "runnable"
			default:
				taskState = "blocked"
			}
		}
		out.hex("task", uint64(task))
		out.word("state", taskState)
	}
	out.uint("free-handles", uint64(owner.Handles().Available()))
	out.end()
	for index := 0; index < handle.MaxHandles; index++ {
		id, typeID, authority, found := owner.Handles().At(index)
		if !found {
			continue
		}
		out.text("DIAG   handle")
		out.hex("id", uint64(id))
		if int(typeID) < len(handleTypeNames) {
			out.word("type", handleTypeNames[typeID])
		}
		out.hex("rights", uint64(authority.Rights()))
		switch typeID {
		case handle.TypeChannel:
			channelID, side, valid := ipc.DecodeEndpointObject(authority.Object())
			out.hex("channel", channelID)
			out.uint("side", uint64(side))
			if channel, exists := channelRegistry.Lookup(channelID); valid && exists {
				out.uint("peer-alive", boolValue(channel.Alive(side^1)))
				out.uint("queued", uint64(channel.Queued(side)))
			} else {
				out.word("channel-state", "released")
			}
		case handle.TypeStream:
			streamID, writer, _ := stream.DecodeEndpointObject(authority.Object())
			out.hex("stream", streamID)
			out.uint("writer", boolValue(writer))
		default:
			out.hex("object", authority.Object())
		}
		out.end()
	}
}

func sideName(side uint8) string {
	if side == 0 {
		return "holder0"
	}
	return "holder1"
}

func boolValue(value bool) uint64 {
	if value {
		return 1
	}
	return 0
}
