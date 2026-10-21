//go:build uefi

package main

// The network function: its discovery, interrupt route, the handles PID 1 is
// given for it, and the reset its driver's teardown performs. The kernel knows
// it only as a device; frames, addresses and protocols are the network
// service's (ADR 0190).

import (
	"goos/internal/capability"
	"goos/kernel/arch/amd64"
	kernelconsole "goos/kernel/console"
	"goos/kernel/device"
	"goos/kernel/handle"
	"goos/kernel/pci"
	"goos/kernel/process"
)

var (
	// virtioNet is the discovered function; virtioNetPort is its I/O base
	// once the function is enabled and its line routed, and zero otherwise.
	virtioNet     pci.Device
	virtioNetPort uint16
	// virtioNetLine is the routed line, which names its IRQ object.
	virtioNetLine uint8
	netCandidate  bool
)

// legacyStatusRegister is the device-status register of the legacy virtio
// header; writing zero to it resets the device.
const legacyStatusRegister = 18

// discoverNetwork finds the network function and enables it. A missing or
// unusable one is reported and leaves the system without a network; nothing
// else depends on it. It runs after the disk's discovery so an overlap with
// the disk's ports can be refused.
func discoverNetwork(block pci.Device, blockFound bool) {
	console := kernelconsole.New()
	found, problem := pci.FindLegacyVirtioNet(pciConfig{})
	reason := ""
	switch {
	case problem == pci.ProblemAbsent:
		console.WriteString("Network: no legacy virtio-net device\n")
		return
	case problem == pci.ProblemDuplicate:
		reason = "more than one virtio-net function"
	case problem == pci.ProblemBAR:
		reason = "unusable I/O BAR"
	case problem == pci.ProblemInterrupt:
		reason = "no INTx line"
	case blockFound && found.Overlaps(block):
		reason = "I/O range overlaps the disk"
	case !netLineSupported(found.IRQ):
		reason = "INTx line shared or not routable"
	}
	if reason != "" {
		console.WriteString("Network: unsupported virtio-net device: ")
		console.WriteString(reason)
		console.WriteString("\n")
		return
	}
	command := uint16(amd64.PCIConfigRead32(found.Bus, found.Slot, found.Function, 4))
	amd64.PCIConfigWrite16(found.Bus, found.Slot, found.Function, 4, pci.EnableCommand(command))
	virtioNet, netCandidate = found, true
	mark("PCI_NET_DISCOVERY_OK")
}

// netLineSupported accepts the ISA lines the Q35 PCI interrupt routers can be
// steered to, without the disk's line 11, the console lines 1 and 4, the
// cascade, the PICs' spurious lines 7 and 15, and the timer, RTC and FPU
// lines. A line shared with another device would need a handler that asks
// each device whether it interrupted, which this kernel does not have.
func netLineSupported(line uint8) bool {
	switch line {
	case 3, 5, 6, 9, 10, 12, 14:
		return true
	}
	return false
}

// routeNetwork programs the network line once the IOAPIC is mapped.
func routeNetwork() {
	if !netCandidate {
		return
	}
	if !amd64.ConfigureNetIRQ(virtioNet.IRQ) {
		console := kernelconsole.New()
		console.WriteString("Network: interrupt route unavailable\n")
		return
	}
	virtioNetPort, virtioNetLine = virtioNet.IOBase, virtioNet.IRQ
}

// netIRQObject is the object of the network line's IRQ capability.
func netIRQObject() (uint64, bool) {
	if virtioNetPort == 0 {
		return 0, false
	}
	return device.IRQObject(virtioNetLine)
}

// installNetHandles gives PID 1 the network function's ports, line and DMA
// domain at the fixed slots after the console handles. Without a function the
// slots hold inert objects with no Transfer right, which is how PID 1 tells
// that there is no network to start a driver for.
func installNetHandles(owner *process.Process) bool {
	portObject, portRights := device.InertObject, capability.Read
	irqObject, irqRights := device.InertIRQObject, capability.Wait
	dmaObject, dmaRights := device.InertObject, capability.Read
	if virtioNetPort != 0 {
		var portValid, irqValid, dmaValid bool
		portObject, portValid = device.PortObject(virtioNet.IOBase, virtioNet.IOSize)
		irqObject, irqValid = netIRQObject()
		dmaObject, dmaValid = device.DMAObject(device.DMADomainNet)
		if !portValid || !irqValid || !dmaValid {
			return false
		}
		portRights = capability.Read | capability.Write | capability.Transfer
		irqRights = capability.Wait | capability.Transfer
		dmaRights = capability.Write | capability.Transfer
	}
	port, portInstalled := owner.Handles().Insert(handle.TypeDevice, capability.New(portObject, portRights))
	irq, irqInstalled := owner.Handles().Insert(handle.TypeIRQ, capability.New(irqObject, irqRights))
	dma, dmaInstalled := owner.Handles().Insert(handle.TypeDevice, capability.New(dmaObject, dmaRights))
	return portInstalled && irqInstalled && dmaInstalled && port == handle.ID(0x000000010000001c) &&
		irq == handle.ID(0x000000010000001d) && dma == handle.ID(0x000000010000001e)
}

// dmaDomainPresent reports whether a DMA domain names a discovered device.
func dmaDomainPresent(domain uint16) bool {
	switch domain {
	case device.DMADomainBlock:
		return virtioBlockPort != 0
	case device.DMADomainNet:
		return virtioNetPort != 0
	}
	return false
}

// quiesceDMADomain stops the device a closed DMA capability belonged to before
// the pages it may still write are freed with its driver's address space: the
// route is masked first, so the reset cannot leave an event behind, then the
// device is reset. Each domain touches only its own device.
func quiesceDMADomain(domain uint16) {
	switch domain {
	case device.DMADomainBlock:
		if virtioBlockPort != 0 {
			amd64.QuiesceIRQ11()
			amd64.PortWrite(virtioBlockPort+legacyStatusRegister, 1, 0)
		}
	case device.DMADomainNet:
		if virtioNetPort != 0 {
			amd64.QuiesceNetIRQ()
			amd64.PortWrite(virtioNetPort+legacyStatusRegister, 1, 0)
			amd64.QuiesceNetIRQ()
		}
	}
}
