//go:build goos_userspace

// Command virtio-net drives the legacy virtio network function for the
// network service (ADR 0190, 0191). It owns the device's ports, line and DMA
// window; frames reach the service through two shared pools, and only their
// slot numbers and lengths travel in GNETDRV1 messages. A device address never
// leaves this process.
package main

import (
	"goos/internal/validation"
	"goos/kernel/clock"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/mm/vm"
	syslog "goos/userspace/log"
	"goos/userspace/netdev"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/sys"
	"goos/userspace/startup"
	userwait "goos/userspace/wait"
)

// Legacy virtio header registers and the network device's configuration.
const (
	registerHostFeatures  = 0
	registerGuestFeatures = 4
	registerQueuePFN      = 8
	registerQueueSize     = 12
	registerQueueSelect   = 14
	registerQueueNotify   = 16
	registerStatus        = 18
	registerISR           = 19
	registerMAC           = 20
	registerLinkStatus    = 26

	statusAcknowledge = 1
	statusDriver      = 2
	statusDriverOK    = 4

	featureMAC    = uint32(1) << 5
	featureStatus = uint32(1) << 16
	linkUp        = 1
	isrConfig     = 2

	receiveQueue  = 0
	transmitQueue = 1
)

const (
	dmaVirtualAddress = vm.UserDMAAddress
	// maxDMAPages is the kernel's limit on one DMA region.
	maxDMAPages = 64
	// receiveBuffers are the device-owned buffers kept posted; each holds the
	// header and one untagged frame.
	receiveBuffers = 16
	bufferSize     = 2048
	// waitTimeout bounds one wait so a lost wake costs seconds, not the
	// driver. The device is not polled when it expires.
	waitTimeout = 10 * clock.TicksPerSecond
)

// Exit codes; none is zero, so the supervisor starts the driver again.
const (
	exitFailed      = 1
	exitServiceGone = 2
)

var (
	consoleHandle, portHandle, irqHandle, dmaHandle, serviceHandle, rxHandle, txHandle handle.ID

	generation uint64
	dma        sys.DMARegion
	rxPool     []byte
	txPool     []byte
	rxQueue    netdev.Queue
	txQueue    netdev.Queue
	// rxBufferOffset and txBufferOffset are where the device-owned packet
	// buffers start in the DMA region.
	rxBufferOffset, txBufferOffset uint64
	// rxBufferOf maps a receive descriptor to its buffer; txSlotOf a transmit
	// descriptor to the pool slot whose frame it carries.
	rxBufferOf [netdev.MaxQueueSize]uint16
	txSlotOf   [netdev.MaxQueueSize]uint16
	// txCorrelation is the request each transmit slot answers.
	txCorrelation [netdev.Slots]uint64
	rxSlots       netdev.Pool
	txSlots       netdev.Pool
	statusFeature bool
	link          netdev.Link
	mac           [6]byte

	wire      [ipc.WireMaxSize]byte
	message   ipc.Message
	frame     netdev.Message
	interests [kcall.WaitAnyMaxObjects]sys.Interest

	counters struct{ rx, tx, drop, malformed uint64 }
)

// mapPool maps one frame pool, which must be at least the protocol's size.
func mapPool(pool handle.ID, writable bool) ([]byte, bool) {
	base, size, status := sys.MemoryMapAnywhere(pool, writable)
	if status != kcall.StatusOK || size < netdev.PoolSize {
		return nil, false
	}
	return sys.Window(base, netdev.PoolSize), true
}

func specification(offset uint16, width uint8) uint64 { return uint64(offset) | uint64(width)<<16 }

func portIn(offset uint16, width uint8) (uint32, bool) {
	value, status := sys.PortRead(portHandle, specification(offset, width))
	return uint32(value), status == kcall.StatusOK
}

func portOut(offset uint16, width uint8, value uint32) bool {
	return sys.PortWrite(portHandle, specification(offset, width), uint64(value)) == kcall.StatusOK
}

//export userspaceMain
func userspaceMain() {
	arena.Init()
	arena.OnExhausted(func() { fatal("VIRTIO_NET_OUT_OF_MEMORY\n") })
	data, ok := startup.Read()
	if !ok {
		fatal("VIRTIO_NET_STARTUP_INVALID\n")
	}
	consoleHandle, ok = data.Handle("console")
	syslog.Init(consoleHandle, "virtio-net")
	for _, binding := range [...]struct {
		name   string
		target *handle.ID
	}{{"ports", &portHandle}, {"irq", &irqHandle}, {"dma", &dmaHandle}, {"service", &serviceHandle}, {"rx", &rxHandle}, {"tx", &txHandle}} {
		if *binding.target, ok = data.Handle(binding.name); !ok {
			fatal("VIRTIO_NET_CAPABILITY_MISSING\n")
		}
	}
	generation = parseGeneration(data)
	if generation == 0 {
		fatal("VIRTIO_NET_GENERATION_MISSING\n")
	}
	rxPool, ok = mapPool(rxHandle, true)
	if ok {
		txPool, ok = mapPool(txHandle, false)
	}
	if !ok {
		fatal("VIRTIO_NET_POOL_FAILED\n")
	}
	rxSlots.Reset(true)
	txSlots.Reset(false)
	if !initializeDevice() {
		portOut(registerStatus, 1, 0)
		fatal("VIRTIO_NET_INIT_FAILED\n")
	}
	if !sendReady() {
		fatal("VIRTIO_NET_READY_FAILED\n")
	}
	mark("VIRTIO_NET_DRIVER_OK\n")
	syslog.Info("device ready", syslog.Uint("generation", generation), syslog.Uint("queue", uint64(rxQueue.Size())), syslog.Uint("link", uint64(link)))
	serve()
}

// parseGeneration reads the binding generation PID 1 put in the startup
// environment; every message this driver sends carries it.
func parseGeneration(data startup.Data) uint64 {
	text, found := data.Env("NET_GENERATION")
	if !found || len(text) == 0 || len(text) > 19 {
		return 0
	}
	value := uint64(0)
	for _, digit := range text {
		if digit < '0' || digit > '9' {
			return 0
		}
		value = value*10 + uint64(digit-'0')
	}
	return value
}

// initializeDevice follows the legacy order: reset, acknowledge, driver,
// features, queues, receive buffers posted, DRIVER_OK, and only then the
// first notification.
func initializeDevice() bool {
	if !portOut(registerStatus, 1, 0) || !portOut(registerStatus, 1, statusAcknowledge) ||
		!portOut(registerStatus, 1, statusAcknowledge|statusDriver) {
		return false
	}
	offered, ok := portIn(registerHostFeatures, 4)
	if !ok || offered&featureMAC == 0 {
		syslog.Error("device does not report its MAC")
		return false
	}
	accepted := featureMAC
	if offered&featureStatus != 0 {
		accepted |= featureStatus
		statusFeature = true
	}
	if !portOut(registerGuestFeatures, 4, accepted) {
		return false
	}
	for index := range mac {
		value, read := portIn(registerMAC+uint16(index), 1)
		if !read {
			return false
		}
		mac[index] = byte(value)
	}
	if !netdev.ValidMAC(mac) {
		syslog.Error("device MAC is not a unicast address")
		return false
	}
	rxSize, rxOK := queueSize(receiveQueue)
	txSize, txOK := queueSize(transmitQueue)
	if !rxOK || !txOK {
		syslog.Error("unusable queue size")
		return false
	}
	_, _, rxTotal, _ := netdev.QueueLayout(rxSize)
	_, _, txTotal, _ := netdev.QueueLayout(txSize)
	rxBufferOffset = rxTotal + txTotal
	txBufferOffset = rxBufferOffset + receiveBuffers*bufferSize
	size := txBufferOffset + netdev.Slots*bufferSize
	size = (size + vm.PageSize - 1) &^ (vm.PageSize - 1)
	if size > maxDMAPages*vm.PageSize {
		syslog.Error("queues exceed the DMA limit")
		return false
	}
	region, status := sys.DMAAllocate(dmaHandle, dmaVirtualAddress, size)
	if status != kcall.StatusOK {
		return false
	}
	dma = region
	if !rxQueue.Init(dma.Memory[:rxTotal], dma.Physical, rxSize) ||
		!txQueue.Init(dma.Memory[rxTotal:rxTotal+txTotal], dma.Physical+rxTotal, txSize) ||
		!activate(receiveQueue, dma.Physical) || !activate(transmitQueue, dma.Physical+rxTotal) {
		return false
	}
	posted := uint16(receiveBuffers)
	if posted > rxSize {
		posted = rxSize
	}
	for buffer := uint16(0); buffer < posted; buffer++ {
		if !postReceive(buffer) {
			return false
		}
	}
	rxQueue.Publish()
	if !portOut(registerStatus, 1, statusAcknowledge|statusDriver|statusDriverOK) || !portOut(registerQueueNotify, 2, receiveQueue) {
		return false
	}
	link = netdev.LinkUp
	if statusFeature {
		link = readLink()
	}
	return true
}

// queueSize reads a queue's size, which must be a power of two the ring
// layout accepts. Queue sizes are the device's to choose; one this driver
// cannot lay out is refused rather than truncated.
func queueSize(queue uint16) (uint16, bool) {
	if !portOut(registerQueueSelect, 2, uint32(queue)) {
		return 0, false
	}
	value, ok := portIn(registerQueueSize, 2)
	if !ok {
		return 0, false
	}
	_, _, _, valid := netdev.QueueLayout(uint16(value))
	return uint16(value), valid && value <= netdev.MaxQueueSize
}

func activate(queue uint16, physical uint64) bool {
	return portOut(registerQueueSelect, 2, uint32(queue)) && portOut(registerQueuePFN, 4, uint32(physical/netdev.QueueAlign))
}

func readLink() netdev.Link {
	value, ok := portIn(registerLinkStatus, 2)
	if ok && value&linkUp != 0 {
		return netdev.LinkUp
	}
	return netdev.LinkDown
}

func postReceive(buffer uint16) bool {
	id, ok := rxQueue.Add(dma.Physical+rxBufferOffset+uint64(buffer)*bufferSize, bufferSize, true)
	if !ok {
		return false
	}
	rxBufferOf[id] = buffer
	return true
}

func serve() {
	for {
		interests[0] = sys.Interest{Handle: serviceHandle, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
		interests[1] = sys.Interest{Handle: irqHandle, Events: kcall.WaitReadable}
		result, status := userwait.Any(&interests, 2, waitTimeout)
		ready := result.ReadyMask
		if status == kcall.StatusTimeout {
			continue
		}
		if status != kcall.StatusOK {
			fatal("VIRTIO_NET_WAIT_FAILED\n")
		}
		if ready&2 != 0 {
			serviceDevice()
		}
		if ready&1 != 0 {
			serviceMessages()
		}
	}
}

// serviceDevice handles one interrupt. The ISR read deasserts the line and
// comes before IRQTake, so an interrupt raised after it is still pending for
// the next wait.
func serviceDevice() {
	isr, ok := portIn(registerISR, 1)
	if !ok {
		fault(netdev.FaultDevice, "VIRTIO_NET_ISR_FAILED\n")
	}
	sys.IRQTake(irqHandle)
	if isr&isrConfig != 0 && statusFeature {
		if current := readLink(); current != link {
			link = current
			syslog.Info("link changed", syslog.Uint("up", uint64(link)))
			if !sendReady() {
				fatal("VIRTIO_NET_READY_FAILED\n")
			}
		}
	}
	completeTransmits()
	deliverReceived()
}

// deliverReceived hands completed receive buffers to the service, one free
// pool slot per frame. With no slot free it stops and leaves the rest in the
// used ring: the device then runs out of posted buffers and drops for itself,
// so a slow service costs frames, not memory.
func deliverReceived() {
	reposted := false
	for {
		slot, free := rxSlots.Acquire()
		if !free {
			break
		}
		id, written, status := rxQueue.Used()
		if status == netdev.UsedNone {
			rxSlots.Release(slot)
			break
		}
		if status != netdev.UsedOK {
			counters.malformed++
			syslog.Error("malformed receive completion", syslog.Uint("status", uint64(status)),
				syslog.Uint("id", uint64(id)), syslog.Uint("written", uint64(written)))
			fault(netdev.FaultDevice, "VIRTIO_NET_MALFORMED_RECEIVE_COMPLETION\n")
		}
		buffer := rxBufferOf[id]
		base := rxBufferOffset + uint64(buffer)*bufferSize
		length, valid := netdev.ValidReceive(written)
		if !valid || !netdev.ValidHeader(dma.Memory[base:base+netdev.NetHeaderSize]) {
			// A device that writes a header it was not asked for or a length
			// outside one frame is not trusted with further buffers.
			counters.malformed++
			syslog.Error("malformed receive frame", syslog.Uint("id", uint64(id)),
				syslog.Uint("written", uint64(written)), syslog.Uint("flags", uint64(dma.Memory[base])),
				syslog.Uint("gso_type", uint64(dma.Memory[base+1])))
			fault(netdev.FaultDevice, "VIRTIO_NET_MALFORMED_RECEIVE_FRAME\n")
		}
		offset, _ := netdev.SlotOffset(slot)
		copy(rxPool[offset:offset+uint64(length)], dma.Memory[base+netdev.NetHeaderSize:base+netdev.NetHeaderSize+uint64(length)])
		if !postReceive(buffer) {
			fault(netdev.FaultDevice, "VIRTIO_NET_REPOST_FAILED\n")
		}
		reposted = true
		frame = netdev.Message{Opcode: netdev.OpRxReady, Generation: generation, Slot: slot, Length: length}
		if send(&frame, 0) {
			rxSlots.Give(slot)
			counters.rx++
		} else {
			rxSlots.Release(slot)
			counters.drop++
		}
	}
	if reposted {
		rxQueue.Publish()
		portOut(registerQueueNotify, 2, receiveQueue)
	}
}

// completeTransmits answers every transmit the device has finished.
func completeTransmits() {
	for {
		id, _, status := txQueue.Used()
		if status == netdev.UsedNone {
			return
		}
		if status != netdev.UsedOK {
			counters.malformed++
			fault(netdev.FaultDevice, "VIRTIO_NET_MALFORMED_TRANSMIT\n")
		}
		finishTransmit(txSlotOf[id], netdev.ResultOK)
		counters.tx++
	}
}

// finishTransmit returns a slot with its result. The reply answers the
// slot's request, so the channel refuses a second one.
func finishTransmit(slot uint16, result netdev.Result) {
	frame = netdev.Message{Opcode: netdev.OpTxDone, Generation: generation, Slot: slot, Result: result}
	correlation := txCorrelation[slot]
	txCorrelation[slot] = 0
	if !send(&frame, correlation) || !txSlots.Give(slot) {
		fatal("VIRTIO_NET_TXDONE_FAILED\n")
	}
}

func serviceMessages() {
	for {
		status := sys.IPCReceive(serviceHandle, wire[:])
		if status == kcall.StatusNotFound {
			return
		}
		if status == kcall.StatusPeerDied {
			// The network service is gone. Its replacement binds a new
			// driver; this one's teardown resets the device.
			syslog.Warn("network service gone")
			sys.Exit(exitServiceGone)
		}
		if status != kcall.StatusOK {
			fatal("VIRTIO_NET_RECEIVE_FAILED\n")
		}
		payloadSize := int(wire[24]) | int(wire[25])<<8
		if !ipc.DecodeInto(wire[:ipc.WireHeaderSize+payloadSize+int(wire[26])*8], &message) {
			fault(netdev.FaultProtocol, "VIRTIO_NET_MESSAGE_INVALID\n")
		}
		correlation, valid := netdev.Decode(&message, &frame)
		if !valid {
			for _, attached := range message.Handles[:message.HandleCount] {
				sys.HandleClose(attached)
			}
			fault(netdev.FaultProtocol, "VIRTIO_NET_MESSAGE_INVALID\n")
		}
		if frame.Generation != generation {
			fault(netdev.FaultGeneration, "VIRTIO_NET_GENERATION_MISMATCH\n")
		}
		switch frame.Opcode {
		case netdev.OpTx:
			transmit(frame.Slot, frame.Length, correlation)
		case netdev.OpRxRelease:
			if !rxSlots.Take(frame.Slot) || !rxSlots.Release(frame.Slot) {
				fault(netdev.FaultProtocol, "VIRTIO_NET_RELEASE_INVALID\n")
			}
			// A freed slot may let a frame waiting in the used ring through.
			deliverReceived()
		case netdev.OpFault:
			syslog.Warn("network service reported a fault", syslog.Uint("reason", uint64(frame.Reason)))
			sys.Exit(exitFailed)
		default:
			fault(netdev.FaultProtocol, "VIRTIO_NET_OPERATION_INVALID\n")
		}
	}
}

// transmit copies a frame out of the shared pool before the device sees it,
// so the service cannot change a buffer the device is reading.
func transmit(slot, length uint16, correlation uint64) {
	if !txSlots.Take(slot) {
		fault(netdev.FaultProtocol, "VIRTIO_NET_TRANSMIT_INVALID\n")
	}
	txCorrelation[slot] = correlation
	source, _ := netdev.SlotOffset(slot)
	base := txBufferOffset + uint64(slot)*bufferSize
	header := dma.Memory[base : base+netdev.NetHeaderSize]
	clear(header)
	copy(dma.Memory[base+netdev.NetHeaderSize:base+netdev.NetHeaderSize+uint64(length)], txPool[source:source+uint64(length)])
	id, added := txQueue.Add(dma.Physical+base, uint32(netdev.NetHeaderSize)+uint32(length), false)
	if !added {
		counters.drop++
		finishTransmit(slot, netdev.ResultBusy)
		return
	}
	txSlotOf[id] = slot
	txQueue.Publish()
	if !portOut(registerQueueNotify, 2, transmitQueue) {
		fault(netdev.FaultDevice, "VIRTIO_NET_NOTIFY_FAILED\n")
	}
}

func sendReady() bool {
	frame = netdev.Message{Opcode: netdev.OpReady, Generation: generation, MTU: netdev.MTU, MAC: mac, Link: link}
	return send(&frame, 0)
}

func send(value *netdev.Message, correlation uint64) bool {
	if !netdev.Encode(value, correlation, &message) {
		return false
	}
	size, ok := ipc.EncodeFrom(&message, wire[:])
	return ok && sys.IPCSend(serviceHandle, wire[:size]) == kcall.StatusOK
}

// fault tells the service why the binding ends, resets the device, and
// exits; the supervisor starts a new driver with a new generation.
func fault(reason netdev.FaultReason, text string) {
	frame = netdev.Message{Opcode: netdev.OpFault, Generation: generation, Reason: reason}
	send(&frame, 0)
	portOut(registerStatus, 1, 0)
	syslog.Error(text[:len(text)-1], syslog.Uint("rx", counters.rx), syslog.Uint("tx", counters.tx),
		syslog.Uint("drop", counters.drop), syslog.Uint("malformed", counters.malformed))
	sys.Exit(exitFailed)
}

func mark(text string) {
	if validation.Enabled {
		sys.ConsoleWriteString(consoleHandle, text)
	}
}

// fatal reports why the driver stops and exits: a driver spinning here would
// hold the device, and the supervisor could not replace it.
func fatal(text string) {
	if !syslog.Error(text[:len(text)-1]) {
		sys.ConsoleWriteString(consoleHandle, text)
	}
	sys.Exit(exitFailed)
}

func main() {}
