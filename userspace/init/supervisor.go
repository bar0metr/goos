// Package supervisor implements the bounded policy core of the init service.
package supervisor

import (
	"goos/internal/capability"
	"goos/kernel/clock"
	"goos/kernel/handle"
)

const (
	MaxServices     = 16
	MaxDependencies = 8
	MaxCapabilities = 7
	MaxEvents       = 32
	MaxName         = 31
	MaxExecutable   = 127
	MaxRestarts     = 3
	RestartWindow   = uint64(1000)
	RestartBackoff  = uint64(clock.TicksPerSecond / 10)
)

type Status uint8

const (
	StatusOK Status = iota
	StatusInvalidArgument
	StatusAlreadyExists
	StatusNotFound
	StatusResourceExhausted
	StatusInvalidConfiguration
)

type RestartPolicy uint8

const (
	RestartNever RestartPolicy = iota
	RestartOnFailure
	RestartAlways
)

type State uint8

const (
	StateRegistered State = iota
	StateBlocked
	StateStarting
	StateRunning
	StateStopped
	StateFailed
)

type Health uint8

const (
	HealthUnknown Health = iota
	HealthBlocked
	HealthStarting
	HealthHealthy
	HealthStopped
	HealthFailed
)

type Capability struct {
	Name    string
	Handle  handle.ID
	Type    handle.Type
	Rights  capability.Rights
	Consume bool
}

type Definition struct {
	Name         string
	Executable   string
	Dependencies []string
	Capabilities []Capability
	Restart      RestartPolicy
}

type LaunchSpec struct {
	Name            [MaxName]byte
	NameSize        uint8
	Executable      [MaxExecutable]byte
	ExecutableSize  uint8
	Capabilities    [MaxCapabilities]Capability
	CapabilityCount uint8
}

type EventKind uint8

const (
	EventRegistered EventKind = iota + 1
	EventStarted
	EventExited
	EventRestartScheduled
	EventRateLimited
	EventDependencyBlocked
)

type Event struct {
	Kind    EventKind
	Service uint8
	PID     uint64
	Detail  uint64
	Tick    uint64
}

type Snapshot struct {
	ID       uint8
	PID      uint64
	State    State
	Health   Health
	Restarts uint8
}

type service struct {
	launch          LaunchSpec
	dependencies    [MaxDependencies]uint8
	dependencyCount uint8
	restart         RestartPolicy
	state           State
	health          Health
	pid             uint64
	windowStart     uint64
	restarts        uint8
	nextStart       uint64
	used            bool
}

type Manager struct {
	services   [MaxServices]service
	count      uint8
	events     [MaxEvents]Event
	eventStart uint8
	eventCount uint8
}

func (manager *Manager) Register(definition Definition) Status {
	if !validName(definition.Name) || !validExecutable(definition.Executable) ||
		len(definition.Dependencies) > MaxDependencies || len(definition.Capabilities) > MaxCapabilities || definition.Restart > RestartAlways {
		return StatusInvalidArgument
	}
	for index, grant := range definition.Capabilities {
		const allRights = capability.Read | capability.Write | capability.Execute | capability.Transfer | capability.Wait
		if !validCapabilityName(grant.Name) || grant.Handle == 0 || grant.Type == handle.TypeInvalid || grant.Rights == 0 || grant.Rights&^allRights != 0 {
			return StatusInvalidArgument
		}
		for previous := 0; previous < index; previous++ {
			if definition.Capabilities[previous].Name == grant.Name {
				return StatusInvalidArgument
			}
		}
	}
	if _, found := manager.index(definition.Name); found {
		return StatusAlreadyExists
	}
	if manager.count == MaxServices {
		return StatusResourceExhausted
	}
	entry := &manager.services[manager.count]
	entry.used = true
	entry.state, entry.health, entry.restart = StateRegistered, HealthUnknown, definition.Restart
	entry.launch.NameSize = uint8(copy(entry.launch.Name[:], definition.Name))
	entry.launch.ExecutableSize = uint8(copy(entry.launch.Executable[:], definition.Executable))
	entry.launch.CapabilityCount = uint8(copy(entry.launch.Capabilities[:], definition.Capabilities))
	for index, dependency := range definition.Dependencies {
		dependencyIndex, found := manager.index(dependency)
		if !found || dependency == definition.Name {
			*entry = service{}
			return StatusInvalidConfiguration
		}
		entry.dependencies[index] = dependencyIndex
		entry.dependencyCount++
	}
	manager.count++
	manager.record(Event{Kind: EventRegistered, Service: manager.count})
	return StatusOK
}

// StartReady starts every service whose dependencies are healthy. The launcher
// is the only process-creation boundary and must install exactly the declared
// capabilities before returning a non-zero PID.
func (manager *Manager) StartReady(now uint64, launcher func(LaunchSpec) (uint64, bool)) int {
	if launcher == nil {
		return 0
	}
	started := 0
	for progress := true; progress; {
		progress = false
		for index := 0; index < int(manager.count); index++ {
			entry := &manager.services[index]
			if entry.state != StateRegistered && entry.state != StateBlocked || now < entry.nextStart {
				continue
			}
			if !manager.dependenciesHealthy(entry) {
				if entry.state != StateBlocked {
					entry.state, entry.health = StateBlocked, HealthBlocked
					manager.record(Event{Kind: EventDependencyBlocked, Service: uint8(index + 1), Tick: now})
				}
				continue
			}
			entry.state, entry.health = StateStarting, HealthStarting
			pid, ok := launcher(entry.launch)
			if !ok || pid == 0 || manager.pidInUse(pid) {
				manager.failed(index, 1, now)
				continue
			}
			entry.pid, entry.state, entry.health = pid, StateRunning, HealthHealthy
			manager.record(Event{Kind: EventStarted, Service: uint8(index + 1), PID: pid, Tick: now})
			manager.refreshRunningHealth(now)
			started++
			progress = true
		}
	}
	return started
}

func (manager *Manager) Exit(pid uint64, code uint64, now uint64) Status {
	if pid == 0 {
		return StatusInvalidArgument
	}
	for index := 0; index < int(manager.count); index++ {
		entry := &manager.services[index]
		if entry.state == StateRunning && entry.pid == pid {
			manager.record(Event{Kind: EventExited, Service: uint8(index + 1), PID: pid, Detail: code, Tick: now})
			entry.pid = 0
			if code == 0 && entry.restart != RestartAlways {
				entry.state, entry.health = StateStopped, HealthStopped
				manager.refreshRunningHealth(now)
				return StatusOK
			}
			if entry.restart == RestartNever {
				entry.state, entry.health = StateFailed, HealthFailed
				manager.refreshRunningHealth(now)
				return StatusOK
			}
			manager.scheduleRestart(index, now)
			manager.refreshRunningHealth(now)
			return StatusOK
		}
	}
	return StatusNotFound
}

// RestartPending reports whether any service is waiting to be started. A
// scheduled restart is a deadline, not an event: nothing else will arrive to
// make it happen, so a supervisor that only wakes on events would leave the
// service down forever.
func (manager *Manager) RestartPending() bool {
	for index := 0; index < int(manager.count); index++ {
		if state := manager.services[index].state; state == StateRegistered || state == StateBlocked {
			return true
		}
	}
	return false
}

// SetCapability replaces the handle of one named capability in a registered
// service's launch spec. A restarted service cannot always be handed the
// endpoints it had: a channel dies with the process that held the other half,
// so its replacement needs a fresh one, and the definition has to say so or the
// next restart would hand out the dead handle again.
func (manager *Manager) SetCapability(name string, capability string, id handle.ID) Status {
	index, found := manager.index(name)
	if !found {
		return StatusNotFound
	}
	if id == 0 {
		return StatusInvalidArgument
	}
	entry := &manager.services[index]
	for slot := 0; slot < int(entry.launch.CapabilityCount); slot++ {
		if entry.launch.Capabilities[slot].Name == capability {
			entry.launch.Capabilities[slot].Handle = id
			return StatusOK
		}
	}
	return StatusNotFound
}

func (manager *Manager) Snapshot(name string) (Snapshot, bool) {
	index, found := manager.index(name)
	if !found {
		return Snapshot{}, false
	}
	entry := &manager.services[index]
	return Snapshot{ID: index + 1, PID: entry.pid, State: entry.state, Health: entry.health, Restarts: entry.restarts}, true
}

// At returns the name and state of the index-th registered service.
func (manager *Manager) At(index int) ([]byte, Snapshot, bool) {
	if index < 0 || index >= int(manager.count) || !manager.services[index].used {
		return nil, Snapshot{}, false
	}
	entry := &manager.services[index]
	return entry.launch.Name[:entry.launch.NameSize], Snapshot{ID: uint8(index + 1), PID: entry.pid, State: entry.state, Health: entry.health, Restarts: entry.restarts}, true
}

func (manager *Manager) Event(index int) (Event, bool) {
	if index < 0 || index >= int(manager.eventCount) {
		return Event{}, false
	}
	return manager.events[(int(manager.eventStart)+index)%MaxEvents], true
}

func (manager *Manager) EventCount() int { return int(manager.eventCount) }

func (manager *Manager) failed(index int, code uint64, now uint64) {
	entry := &manager.services[index]
	if entry.restart == RestartNever {
		entry.state, entry.health = StateFailed, HealthFailed
		return
	}
	manager.scheduleRestart(index, now)
}

func (manager *Manager) scheduleRestart(index int, now uint64) {
	entry := &manager.services[index]
	if entry.restarts == 0 || now < entry.windowStart || now-entry.windowStart >= RestartWindow {
		entry.windowStart, entry.restarts = now, 0
	}
	if entry.restarts >= MaxRestarts {
		entry.state, entry.health = StateFailed, HealthFailed
		manager.record(Event{Kind: EventRateLimited, Service: uint8(index + 1), Tick: now})
		return
	}
	entry.restarts++
	entry.nextStart = now + RestartBackoff
	entry.state, entry.health = StateRegistered, HealthStarting
	manager.record(Event{Kind: EventRestartScheduled, Service: uint8(index + 1), Detail: uint64(entry.restarts), Tick: now})
}

func (manager *Manager) dependenciesHealthy(entry *service) bool {
	for index := 0; index < int(entry.dependencyCount); index++ {
		dependency := &manager.services[entry.dependencies[index]]
		if dependency.state != StateRunning || dependency.health != HealthHealthy {
			return false
		}
	}
	return true
}

func (manager *Manager) pidInUse(pid uint64) bool {
	for index := 0; index < int(manager.count); index++ {
		if manager.services[index].state == StateRunning && manager.services[index].pid == pid {
			return true
		}
	}
	return false
}

func (manager *Manager) refreshRunningHealth(now uint64) {
	for index := 0; index < int(manager.count); index++ {
		entry := &manager.services[index]
		if entry.state != StateRunning {
			continue
		}
		health := HealthHealthy
		if !manager.dependenciesHealthy(entry) {
			health = HealthBlocked
		}
		if entry.health != health && health == HealthBlocked {
			manager.record(Event{Kind: EventDependencyBlocked, Service: uint8(index + 1), PID: entry.pid, Tick: now})
		}
		entry.health = health
	}
}

func (manager *Manager) index(name string) (uint8, bool) {
	for index := 0; index < int(manager.count); index++ {
		entry := &manager.services[index]
		if len(name) != int(entry.launch.NameSize) {
			continue
		}
		match := true
		for offset := range name {
			if entry.launch.Name[offset] != name[offset] {
				match = false
				break
			}
		}
		if match {
			return uint8(index), true
		}
	}
	return 0, false
}

func (manager *Manager) record(event Event) {
	if manager.eventCount < MaxEvents {
		manager.events[(int(manager.eventStart)+int(manager.eventCount))%MaxEvents] = event
		manager.eventCount++
		return
	}
	manager.events[manager.eventStart] = event
	manager.eventStart = (manager.eventStart + 1) % MaxEvents
}

func validName(value string) bool {
	if len(value) == 0 || len(value) > MaxName {
		return false
	}
	for index := range value {
		if value[index] == 0 || value[index] == '/' {
			return false
		}
	}
	return true
}

func validCapabilityName(value string) bool {
	if len(value) == 0 || len(value) > 15 {
		return false
	}
	for index := range value {
		character := value[index]
		if character < 'a' || character > 'z' {
			return false
		}
	}
	return true
}

func validExecutable(value string) bool {
	if len(value) < 2 || len(value) > MaxExecutable || value[0] != '/' {
		return false
	}
	for index := range value {
		if value[index] == 0 {
			return false
		}
	}
	return true
}
