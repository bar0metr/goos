package ipc

import (
	"goos/internal/capability"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/kernel/lock"
)

const (
	QueueCapacity      = 16
	MaxPayload         = 256
	MaxAttachedHandles = 4
)

type Status = kcall.Status

const (
	StatusOK                = kcall.StatusOK
	StatusInvalidArgument   = kcall.StatusInvalidArgument
	StatusPermissionDenied  = kcall.StatusPermissionDenied
	StatusNotFound          = kcall.StatusNotFound
	StatusAlreadyExists     = kcall.StatusAlreadyExists
	StatusResourceExhausted = kcall.StatusResourceExhausted
	StatusTimeout           = kcall.StatusTimeout
	StatusPeerDied          = kcall.StatusPeerDied
)

type Kind uint8

const (
	KindRequest Kind = iota + 1
	KindReply
	KindNotification
)

type Message struct {
	ProtocolID  uint64
	Version     uint16
	Kind        Kind
	Correlation uint64
	Payload     [MaxPayload]byte
	PayloadSize uint16
	Handles     [MaxAttachedHandles]handle.ID
	HandleCount uint8
}

type Transfer struct {
	Handle handle.ID
	Type   handle.Type
	Rights capability.Rights
}

type queue struct {
	messages [QueueCapacity]Message
	head     uint8
	tail     uint8
	count    uint8
}

// Channel is one bidirectional channel. Its lock guards its queues, its
// outstanding requests, and which sides are open.
type Channel struct {
	lock    lock.Spin
	inbound [2]queue
	alive   [2]bool
	pending [2][QueueCapacity]uint64
}

// NewChannel returns an open channel outside any registry.
func NewChannel() *Channel {
	return &Channel{alive: [2]bool{true, true}}
}

// Reset reopens a registry slot in place; see stream.Stream.Reset. Nobody
// else can reach the channel yet.
func (channel *Channel) Reset() {
	*channel = Channel{}
	channel.alive = [2]bool{true, true}
}

// Lock takes the channel's lock.
func (channel *Channel) Lock() {
	channel.lock.SetLevel(lock.LevelChannel)
	channel.lock.Lock()
}

// Unlock releases the channel's lock.
func (channel *Channel) Unlock() { channel.lock.Unlock() }

// held stops a validation kernel whose caller does not hold the lock.
func (channel *Channel) held() {
	if channel != nil {
		channel.lock.AssertHeld()
	}
}

func (channel *Channel) Alive(side uint8) bool {
	channel.held()
	return channel != nil && side <= 1 && channel.alive[side]
}

// Send validates and queues one typed message; see SendFrom.
func (channel *Channel) Send(side uint8, message Message, source *handle.Table, target *handle.Table, transfers []Transfer) Status {
	return channel.SendFrom(side, &message, source, target, transfers)
}

// SendFrom validates and queues one typed message. Side identifies the
// sender; side zero and side one are the only endpoints. The caller holds
// the channel's lock and the handle tables the transfers touch. The message
// is passed by reference, because a copy on a kernel stack costs as much as
// the message, and SendFrom writes the received handles into it.
func (channel *Channel) SendFrom(side uint8, message *Message, source *handle.Table, target *handle.Table, transfers []Transfer) Status {
	channel.held()
	if channel == nil || side > 1 || !validMessage(message) || len(transfers) > MaxAttachedHandles || message.HandleCount != 0 {
		return StatusInvalidArgument
	}
	peer := side ^ 1
	if !channel.alive[side] || !channel.alive[peer] {
		return StatusPeerDied
	}
	queue := &channel.inbound[peer]
	if queue.count == QueueCapacity {
		return StatusResourceExhausted
	}
	if message.Kind == KindRequest {
		if channel.hasPending(side, message.Correlation) {
			return StatusAlreadyExists
		}
	} else if message.Kind == KindReply && !channel.hasPending(peer, message.Correlation) {
		return StatusNotFound
	}

	if len(transfers) != 0 && (source == nil || target == nil) {
		return StatusInvalidArgument
	}
	if target != nil && target.Available() < len(transfers) {
		return StatusResourceExhausted
	}
	for index, transfer := range transfers {
		received, ok := source.Transfer(target, transfer.Handle, transfer.Type, transfer.Rights)
		if !ok {
			for rollback := 0; rollback < index; rollback++ {
				target.Close(message.Handles[rollback])
			}
			return StatusPermissionDenied
		}
		message.Handles[index] = received
		message.HandleCount++
	}
	if message.Kind == KindRequest && !channel.addPending(side, message.Correlation) {
		for rollback := 0; rollback < int(message.HandleCount); rollback++ {
			target.Close(message.Handles[rollback])
		}
		return StatusResourceExhausted
	}
	if message.Kind == KindReply {
		channel.removePending(peer, message.Correlation)
	}
	queue.messages[queue.tail] = *message
	queue.tail = (queue.tail + 1) % QueueCapacity
	queue.count++
	return StatusOK
}

// Receive takes the next message at side; see ReceiveInto.
func (channel *Channel) Receive(side uint8) (Message, Status) {
	var message Message
	status := channel.ReceiveInto(side, &message)
	return message, status
}

// ReceiveInto takes the next message at side into out, or clears out when
// there is none. The caller holds the channel's lock.
func (channel *Channel) ReceiveInto(side uint8, out *Message) Status {
	channel.held()
	if channel == nil || side > 1 || !channel.alive[side] {
		*out = Message{}
		return StatusInvalidArgument
	}
	queue := &channel.inbound[side]
	if queue.count == 0 {
		*out = Message{}
		if !channel.alive[side^1] {
			return StatusPeerDied
		}
		return StatusNotFound
	}
	slot := &queue.messages[queue.head]
	*out = *slot
	*slot = Message{}
	queue.head = (queue.head + 1) % QueueCapacity
	queue.count--
	return StatusOK
}

// ReceiveStatus observes endpoint readiness without consuming a message.
// WaitAny uses it after registering interest so the observation and the
// subsequent decision to block cannot lose an intervening state change.
func (channel *Channel) ReceiveStatus(side uint8) Status {
	channel.held()
	if channel == nil || side > 1 || !channel.alive[side] {
		return StatusInvalidArgument
	}
	if channel.inbound[side].count != 0 {
		return StatusOK
	}
	if !channel.alive[side^1] {
		return StatusPeerDied
	}
	return StatusNotFound
}

// Queued reports how many messages wait at the given endpoint.
func (channel *Channel) Queued(side uint8) int {
	channel.held()
	if channel == nil || side > 1 {
		return 0
	}
	return int(channel.inbound[side].count)
}

// Pending reports how many requests sent from side still await a reply.
func (channel *Channel) Pending(side uint8) int {
	channel.held()
	if channel == nil || side > 1 {
		return 0
	}
	count := 0
	for _, correlation := range channel.pending[side] {
		if correlation != 0 {
			count++
		}
	}
	return count
}

func (channel *Channel) Close(side uint8) bool {
	channel.held()
	if channel == nil || side > 1 || !channel.alive[side] {
		return false
	}
	channel.alive[side] = false
	return true
}

func validMessage(message *Message) bool {
	if message == nil || message.ProtocolID == 0 || message.Version == 0 || message.PayloadSize > MaxPayload || message.HandleCount > MaxAttachedHandles {
		return false
	}
	switch message.Kind {
	case KindRequest, KindReply:
		return message.Correlation != 0
	case KindNotification:
		return message.Correlation == 0
	default:
		return false
	}
}

func (channel *Channel) hasPending(side uint8, correlation uint64) bool {
	for _, value := range channel.pending[side] {
		if value == correlation {
			return true
		}
	}
	return false
}

func (channel *Channel) addPending(side uint8, correlation uint64) bool {
	for index, value := range channel.pending[side] {
		if value == 0 {
			channel.pending[side][index] = correlation
			return true
		}
	}
	return false
}

func (channel *Channel) removePending(side uint8, correlation uint64) {
	for index, value := range channel.pending[side] {
		if value == correlation {
			channel.pending[side][index] = 0
			return
		}
	}
}
