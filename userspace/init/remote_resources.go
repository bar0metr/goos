package supervisor

import (
	"goos/kernel/handle"
	execproto "goos/userspace/exec"
)

const (
	// RemoteJobs bounds all remote pipelines together, not each SSH connection.
	RemoteJobs = 4
	// Leave room for service restarts and kernel tasks outside PID 1's inventory.
	RemoteTaskLimit = 56
)

// RemoteJobAllowed reserves every pipeline stage and one launch child before
// an asynchronous executable read starts. local jobs retain their own slots.
func RemoteJobAllowed(jobs, reservedTasks, stages int, launchesChild bool) bool {
	if jobs < 0 || jobs >= RemoteJobs || stages < 1 || stages > execproto.MaxStages || reservedTasks < 0 {
		return false
	}
	if launchesChild {
		stages++
	}
	return reservedTasks+stages <= RemoteTaskLimit
}

// RemoteResources are PID 1's references after every process and job is reaped.
// Process-owned brokers and executable sources must be released before this.
type RemoteResources struct {
	Handles              [10]handle.ID
	Mapping, MappingSize uint64
}

type RemoteResourceReleaser interface {
	Close(handle.ID) bool
	Unmap(uint64, uint64) bool
}

// Release attempts every release even if one fails. The caller must not recycle
// its registry slot when Release reports an outstanding resource.
func (resources *RemoteResources) Release(releaser RemoteResourceReleaser) bool {
	if releaser == nil {
		return false
	}
	complete := true
	if resources.Mapping != 0 {
		if resources.MappingSize == 0 || !releaser.Unmap(resources.Mapping, resources.MappingSize) {
			complete = false
		} else {
			resources.Mapping, resources.MappingSize = 0, 0
		}
	}
	for index, id := range resources.Handles {
		if id == 0 {
			continue
		}
		if releaser.Close(id) {
			resources.Handles[index] = 0
		} else {
			complete = false
		}
	}
	return complete
}

// RemoteGrace retains worker ownership while its already-reaped shell's output
// drains. Repeated EOF/exit events cannot extend the deadline or change owners.
type RemoteGrace struct {
	Session       uint64
	Code          uint64
	Deadline      uint64
	NoticePending bool
	Retry         uint64
}

func (grace *RemoteGrace) Begin(session, code, now, timeout uint64) bool {
	if grace.Session != 0 || session == 0 || timeout == 0 || now > ^uint64(0)-timeout {
		return false
	}
	grace.Session, grace.Code, grace.Deadline = session, code, now+timeout
	grace.NoticePending, grace.Retry = true, now
	return true
}
func (grace *RemoteGrace) Expired(session, now uint64) bool {
	return session != 0 && grace.Session == session && now >= grace.Deadline
}
func (grace *RemoteGrace) Delivered(session uint64) bool {
	if session == 0 || grace.Session != session {
		return false
	}
	grace.NoticePending = false
	return true
}
