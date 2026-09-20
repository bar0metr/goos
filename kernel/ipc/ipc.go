package ipc

import (
	"goos/internal/capability"
	"goos/kernel/handle"
	"goos/kernel/kcall"
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

type Channel struct {
	inbound [2]queue
	alive   [2]bool
	pending [2][QueueCapacity]uint64
}

func NewChannel() Channel {
	return Channel{alive: [2]bool{true, true}}
}

func (channel *Channel) Alive(side uint8) bool {
	return channel != nil && side <= 1 && channel.alive[side]
}

// Send validates and queues one typed message. Side identifies the sender;
// side zero and side one are the only endpoints. External IRQ-level locking is
// required until channels gain per-object synchronization for SMP.
func (channel *Channel) Send(side uint8, message Message, source *handle.Table, target *handle.Table, transfers []Transfer) Status {
	if channel == nil || side > 1 || !validMessage(&message) || len(transfers) > MaxAttachedHandles || message.HandleCount != 0 {
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
	queue.messages[queue.tail] = message
	queue.tail = (queue.tail + 1) % QueueCapacity
	queue.count++
	return StatusOK
}

func (channel *Channel) Receive(side uint8) (Message, Status) {
	if channel == nil || side > 1 || !channel.alive[side] {
		return Message{}, StatusInvalidArgument
	}
	queue := &channel.inbound[side]
	if queue.count == 0 {
		if !channel.alive[side^1] {
			return Message{}, StatusPeerDied
		}
		return Message{}, StatusNotFound
	}
	message := queue.messages[queue.head]
	queue.messages[queue.head] = Message{}
	queue.head = (queue.head + 1) % QueueCapacity
	queue.count--
	return message, StatusOK
}

// ReceiveStatus observes endpoint readiness without consuming a message.
// WaitAny uses it after registering interest so the observation and the
// subsequent decision to block cannot lose an intervening state change.
func (channel *Channel) ReceiveStatus(side uint8) Status {
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

func (channel *Channel) Close(side uint8) bool {
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
