//go:build uefi && amd64

package amd64

import "sync/atomic"

// A processor holds translations of a process root only while it runs in
// ring 3 on that root: every kernel entry loads the kernel root, and a CR3
// load drops every translation of the root it replaces, since GoOS maps no
// global pages and uses no PCIDs. The kernel itself reaches user memory
// through the direct map, never through a process root.
//
// So a TLB shootdown has one job: make every processor that may be in ring
// 3 on the root enter the kernel once. Each processor publishes the root it
// is about to return to (GOOS_ACTIVATE_ROOT) before the CR3 write, and
// counts its entries (GOOS_ENTER_LOW). The initiator changes the page
// tables, reads which processors run the root, interrupts them, and waits
// until each has entered the kernel or left the root.

var (
	shootdownFence    atomic.Uint64
	shootdowns        atomic.Uint64
	shootdownTargets  atomic.Uint64
	shootdownTimeouts atomic.Uint64
)

// shootdownSpins bounds the wait for one shootdown: interrupts reach a
// processor in ring 3 in microseconds.
const shootdownSpins = 1 << 26

// ShootdownRoot makes sure no processor other than the caller holds a
// translation of root made before the call. The caller has finished
// changing root's page tables and holds the kernel lock, which every change
// to a process root is made under; it frees pages the changed entries named
// only after this returns.
func ShootdownRoot(root uint64) {
	if root == 0 || smpReady == 0 {
		return
	}
	self := CPUIndex()
	targets := onlineMask() &^ (uint64(1) << uint(self))
	if targets == 0 {
		return
	}
	// The locked add orders the page-table stores before the reads below.
	shootdownFence.Add(1)
	var seen [MaxCPUs]uint64
	pending := uint64(0)
	for cpu := 0; cpu < MaxCPUs; cpu++ {
		if targets&(uint64(1)<<uint(cpu)) == 0 || atomic.LoadUint64(&cpuData[cpu][cpuActiveRoot]) != root {
			continue
		}
		seen[cpu] = atomic.LoadUint64(&cpuData[cpu][cpuEntrySeq])
		pending |= uint64(1) << uint(cpu)
	}
	if pending == 0 {
		return
	}
	shootdowns.Add(1)
	for cpu := 0; cpu < MaxCPUs; cpu++ {
		if pending&(uint64(1)<<uint(cpu)) != 0 {
			shootdownTargets.Add(1)
			sendIPI(cpu, shootdownVector)
		}
	}
	for spins := 0; pending != 0; spins++ {
		for cpu := 0; cpu < MaxCPUs; cpu++ {
			bit := uint64(1) << uint(cpu)
			if pending&bit == 0 {
				continue
			}
			if atomic.LoadUint64(&cpuData[cpu][cpuActiveRoot]) != root || atomic.LoadUint64(&cpuData[cpu][cpuEntrySeq]) != seen[cpu] {
				pending &^= bit
			}
		}
		if spins == shootdownSpins {
			shootdownTimeouts.Add(1)
			fatal("TLB shootdown not acknowledged")
		}
		cpuPause()
	}
}

// ShootdownStats reports how many shootdowns interrupted another processor,
// how many interrupts they sent, and how many timed out.
func ShootdownStats() (uint64, uint64, uint64) {
	return shootdowns.Load(), shootdownTargets.Load(), shootdownTimeouts.Load()
}
