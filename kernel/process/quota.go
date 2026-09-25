package process

// Capability isolation decides whether a process may use a resource at all. It
// says nothing about how much of one it may hold, so a process entitled to a
// resource can still take the system's whole supply of it and deny it to
// everyone else. A quota is the second half of that answer: each process gets a
// share of each bounded kernel table, and the kernel refuses a request that
// would take more.
//
// Quotas are not capabilities and are not transferable. A process cannot raise
// its own, and nothing it holds lets it raise another's.

// Resource names a bounded kernel resource charged to the process that holds it.
type Resource uint8

const (
	// ResourceChannels counts IPC channels the process created and that have
	// not been released by the registry.
	ResourceChannels Resource = iota
	// ResourceStreams counts byte streams on the same basis.
	ResourceStreams
	// ResourceTasks counts schedulable tasks bound to the process.
	ResourceTasks
	// ResourcePages counts physical pages the kernel allocated on the
	// process's behalf: its address space and image at launch, and whatever it
	// later asks the kernel to allocate for it.
	ResourcePages
	resourceCount
)

// Limits for an ordinary process. Each is a share of the corresponding global
// table rather than the whole of it, so several processes can hold their full
// quota at once and one cannot starve the rest.
//
// Streams are the limit that binds today: a utility that creates them in a loop
// used to drain the table and break every later pipeline. Four is a share of
// the sixty-four the registry holds (ADR 0089).
//
// Pages count every physical page a process's address space holds: its image
// pages, page tables, and the heap, stack, and anonymous pages it has touched
// (ADR 0104). Reservations cost nothing until touched, so the limit bounds
// memory actually used: 32 MiB, room for a full default stack, a large heap,
// and multi-megabyte buffers.
const (
	defaultChannelLimit = 4
	defaultStreamLimit  = 4
	defaultTaskLimit    = 4
	defaultPageLimit    = 8192
)

// PID 1 multiplexes on behalf of every other process: it creates the channel
// and stream endpoints its children communicate through, so its share is the
// rest of each table rather than an equal slice. This is a larger quota, not a
// waiver: PID 1 is refused past it like anything else.
//
// Only PID 1 can create channels, so its channel share is the whole table. Its
// stream share covers the most it ever holds at once — four streams for each
// of the four job slots and the terminal's output stream — with room to spare,
// and leaves the rest of the registry to everyone else (ADR 0089).
const (
	systemChannelLimit = 24
	systemStreamLimit  = 24
	systemTaskLimit    = 8
	systemPageLimit    = 16384
)

// Quota is a process's resource account. Its counters only ever move through
// Charge and Release, so a leak shows up as a process that cannot acquire
// rather than as a table that quietly fills.
type Quota struct {
	limits [resourceCount]uint32
	used   [resourceCount]uint32
}

func defaultQuota() Quota {
	var quota Quota
	quota.limits[ResourceChannels] = defaultChannelLimit
	quota.limits[ResourceStreams] = defaultStreamLimit
	quota.limits[ResourceTasks] = defaultTaskLimit
	quota.limits[ResourcePages] = defaultPageLimit
	return quota
}

// Quota exposes the account for reporting. Charging goes through the process so
// that no caller can hold a pointer to another process's counters.
func (process *Process) Quota() (used [4]uint32, limits [4]uint32) {
	for index := Resource(0); index < resourceCount; index++ {
		used[index], limits[index] = process.quota.used[index], process.quota.limits[index]
	}
	return used, limits
}

// GrantSystemQuota raises a process to the system share. It is applied to PID 1
// at boot and refuses any other process, so the larger limits cannot be
// acquired later by a process that manages to look like an administrator.
func (process *Process) GrantSystemQuota() bool {
	if process == nil || process.id != 1 {
		return false
	}
	process.quota.limits[ResourceChannels] = systemChannelLimit
	process.quota.limits[ResourceStreams] = systemStreamLimit
	process.quota.limits[ResourceTasks] = systemTaskLimit
	process.quota.limits[ResourcePages] = systemPageLimit
	return true
}

// Charge reserves amount of a resource and reports whether the quota allowed
// it. A refused charge changes nothing, so a caller that checks the result
// never has to unwind a partial reservation.
func (process *Process) Charge(resource Resource, amount uint32) bool {
	if process == nil || resource >= resourceCount || amount == 0 {
		return false
	}
	used := process.quota.used[resource]
	if amount > process.quota.limits[resource]-used {
		return false
	}
	process.quota.used[resource] = used + amount
	return true
}

// Available is how much more of a resource the quota allows.
func (process *Process) Available(resource Resource) uint32 {
	if process == nil || resource >= resourceCount {
		return 0
	}
	return process.quota.limits[resource] - process.quota.used[resource]
}

// Release returns amount of a resource. It reports false, and releases what
// there is, when asked to return more than is held: that is a kernel accounting
// bug, and the counter must stay truthful rather than wrap.
func (process *Process) Release(resource Resource, amount uint32) bool {
	if process == nil || resource >= resourceCount || amount == 0 {
		return false
	}
	used := process.quota.used[resource]
	if amount > used {
		process.quota.used[resource] = 0
		return false
	}
	process.quota.used[resource] = used - amount
	return true
}

// ReleaseAll drops every reservation. Process teardown calls it after the
// resources themselves are gone, so a slot reused by a later process starts
// from a clean account.
func (process *Process) ReleaseAll() {
	if process == nil {
		return
	}
	for index := range process.quota.used {
		process.quota.used[index] = 0
	}
}

// AccountCPU adds elapsed run time to the process. The scheduler charges the
// process whose task was running at each timer tick, so the total is a sample
// count rather than an exact duration, which is what a tick-driven scheduler
// can honestly report.
func (process *Process) AccountCPU(ticks uint64) {
	if process == nil {
		return
	}
	process.cpuTicks += ticks
}

func (process *Process) CPUTicks() uint64 {
	if process == nil {
		return 0
	}
	return process.cpuTicks
}

// ChargeProcess charges a resource to the process owning an address space, and
// reports false when that process is unknown or over quota. Native call
// handlers use it so that an unowned root can never acquire anything.
func (registry *Registry) ChargeProcess(id ID, resource Resource, amount uint32) bool {
	owner, found := registry.ByID(id)
	return found && owner.Charge(resource, amount)
}

// ReleaseProcess returns a resource to a process that may already have been
// removed from the registry, which is the ordinary case for a channel released
// after its creator exited.
func (registry *Registry) ReleaseProcess(id ID, resource Resource, amount uint32) bool {
	owner, found := registry.ByID(id)
	return found && owner.Release(resource, amount)
}
