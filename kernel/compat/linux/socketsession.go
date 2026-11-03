package linux

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/process"
	"goos/kernel/sched"
	"goos/userspace/linuxsockapi"
)

// The family network context (ADR 0210). One Linux family has at most one
// GLNXSOCK1 session: a channel and a pool PID 1 granted its launch, or gave
// it later with LinuxNetAttach. Their handles live in the handle table of
// the process that received them, the launch root; every process and thread
// of the family uses the session through this context, which caches the
// channel's identity and the pool object's and checks both on every use.
//
// The context also holds what the session's exchanges share: the
// correlation counter, the requests in flight with the replies that came
// for them, the RX slots the kernel holds, the Close requests still to
// send, and the family's socket table. Each function here is one step under
// the tables lock; the kernel sends and receives messages between steps,
// with the lock released.

const (
	// MaxSocketRequests bounds the requests one family has in flight. Each
	// owns the TX slot with its index, so a Write always finds its slot.
	MaxSocketRequests = linuxsockapi.TXSlots
	// MaxSockets bounds the sockets of one family.
	MaxSockets = linuxsockapi.MaxSockets
	// socketWaiterSlots bounds the tasks that wait for a request slot.
	socketWaiterSlots = 16
	// pendingCloseSlots holds the Close requests a full queue deferred: one
	// per socket and one per request whose reply named a socket nobody
	// installs.
	pendingCloseSlots = MaxSockets + MaxSocketRequests
	// SocketSleepCorrelation marks the continuation of a socket call that
	// sleeps on readiness. Only such a sleep is interrupted by a signal.
	// Request correlations start above it.
	SocketSleepCorrelation = uint64(1)
	firstSocketCorrelation = uint64(2)
)

// NetSocket is one Linux socket of a family: the service's identity of it
// and the state Linux semantics keep in the kernel.
type NetSocket struct {
	Identity   uint64
	Session    uint32
	Datagram   bool
	Listening  bool
	ReuseAddr  bool
	ShutRead   bool
	ShutWrite  bool
	Connecting bool
	// RecvTimeout and SendTimeout are SO_RCVTIMEO and SO_SNDTIMEO in
	// nanoseconds; zero waits without limit.
	RecvTimeout uint64
	SendTimeout uint64
	generation  uint32
	used        bool
}

type requestState uint8

const (
	requestFree requestState = iota
	// requestSent is in flight with an owner waiting for it.
	requestSent
	// requestReplied holds a reply its owner has yet to take.
	requestReplied
	// requestAbandoned is in flight with nobody waiting; its reply is
	// applied when it comes.
	requestAbandoned
)

type socketRequest struct {
	correlation uint64
	task        sched.TaskID
	socket      uint64
	deadline    uint64
	opcode      linuxsockapi.Opcode
	flags       uint8
	state       requestState
	reply       linuxsockapi.Reply
}

type heldSlot struct {
	socket uint64
	length uint32
	offset uint32
	held   bool
	// unsent marks a Release that could not be queued yet.
	unsent bool
}

// NetEndpoint is what one exchange needs of the session.
type NetEndpoint struct {
	Session    uint32
	Holder     process.ID
	Channel    uint64
	Side       uint8
	PoolObject uint64
}

// NetContext is one family's network context.
type NetContext struct {
	familyID     uint64
	endpoint     NetEndpoint
	channelGrant handle.ID
	poolGrant    handle.ID
	attached     bool
	failed       bool
	correlation  uint64
	requests     [MaxSocketRequests]socketRequest
	sockets      [MaxSockets]NetSocket
	rx           [linuxsockapi.RXSlots]heldSlot
	closes       [pendingCloseSlots]uint64
	closeCount   uint8
	waiters      [socketWaiterSlots]sched.TaskID
	waiterCount  uint8
	nextSocketGn uint32
}

var netContexts [process.MaxProcesses]NetContext

// SocketObject is the Object of an FDSocket description: the context, the
// socket's slot, and the slot's generation, so a reused slot never matches
// an old description.
func socketObject(context int, slot int, generation uint32) uint64 {
	return 1<<63 | uint64(generation&0xffffff)<<16 | uint64(context&0xff)<<8 | uint64(slot&0xff)
}

func splitSocketObject(object uint64) (context int, slot int, generation uint32, ok bool) {
	context, slot, generation = int(object>>8&0xff), int(object&0xff), uint32(object>>16&0xffffff)
	return context, slot, generation, object>>63 == 1 && context < len(netContexts) && slot < MaxSockets
}

func (context *NetContext) reset(familyID uint64) {
	context.familyID = familyID
	context.endpoint = NetEndpoint{}
	context.channelGrant, context.poolGrant = 0, 0
	context.attached, context.failed = false, false
	context.correlation = firstSocketCorrelation - 1
	clear(context.requests[:])
	clear(context.sockets[:])
	clear(context.rx[:])
	clear(context.closes[:])
	context.closeCount = 0
	clear(context.waiters[:])
	context.waiterCount = 0
}

func netContextHeld(familyID uint64) (*NetContext, int, bool) {
	guard.Check()
	if familyID != 0 {
		for index := 0; index < len(netContexts); index++ {
			if netContexts[index].familyID == familyID {
				return &netContexts[index], index, true
			}
		}
	}
	return nil, 0, false
}

// installNetContextHeld gives a new family its context, without a session.
// A context whose family no longer has a VFS context is left over and is
// taken as free; the family's own VFS context exists already.
func installNetContextHeld(familyID uint64) bool {
	guard.Check()
	if familyID == 0 {
		return false
	}
	for index := 0; index < len(netContexts); index++ {
		context := &netContexts[index]
		if context.familyID == familyID {
			context.reset(familyID)
			return true
		}
	}
	for index := 0; index < len(netContexts); index++ {
		context := &netContexts[index]
		if _, live := vFSContextHeld(context.familyID); context.familyID == 0 || !live {
			context.reset(familyID)
			return true
		}
	}
	return false
}

// releaseNetContextHeld forgets a family that ended. Its handles went with
// the processes that held them.
func releaseNetContextHeld(familyID uint64) {
	if context, _, found := netContextHeld(familyID); found {
		context.reset(0)
	}
}

// NetGrant is a session as the kernel resolved it from two handles.
type NetGrant struct {
	Holder        process.ID
	ChannelHandle handle.ID
	PoolHandle    handle.ID
	Channel       uint64
	Side          uint8
	PoolObject    uint64
}

// AttachNet installs a session in a family's context and returns the
// handles of the session it replaces, which the caller closes in holder's
// table. A session that has not failed is not replaced. Everything of the
// old session goes: its sockets fail, its requests and held slots are
// forgotten, since the service that knew them is gone with its channel.
func AttachNet(familyID uint64, grant NetGrant, replaceLive bool) (old NetGrant, replaced bool, ok bool) {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	context, _, found := netContextHeld(familyID)
	if !found || grant.Channel == 0 || grant.PoolObject == 0 || grant.Holder == 0 {
		return NetGrant{}, false, false
	}
	if context.attached && !context.failed && !replaceLive {
		return NetGrant{}, false, false
	}
	if context.attached {
		old = NetGrant{Holder: context.endpoint.Holder, ChannelHandle: context.channelGrant, PoolHandle: context.poolGrant,
			Channel: context.endpoint.Channel, Side: context.endpoint.Side, PoolObject: context.endpoint.PoolObject}
		replaced = true
	}
	session := context.endpoint.Session + 1
	if session == 0 {
		session = 1
	}
	context.endpoint = NetEndpoint{Session: session, Holder: grant.Holder, Channel: grant.Channel, Side: grant.Side, PoolObject: grant.PoolObject}
	context.channelGrant, context.poolGrant = grant.ChannelHandle, grant.PoolHandle
	context.attached, context.failed = true, false
	clear(context.requests[:])
	clear(context.rx[:])
	clear(context.closes[:])
	context.closeCount = 0
	noteNetHeld(&netDiagnostics.Attaches)
	return old, replaced, true
}

// NetSession returns the live session of a family. failed reports a session
// that existed and failed; neither means the family never had one.
func NetSession(familyID uint64) (endpoint NetEndpoint, live bool, failed bool) {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	context, _, found := netContextHeld(familyID)
	if !found || !context.attached {
		return NetEndpoint{}, false, false
	}
	if context.failed {
		return context.endpoint, false, true
	}
	return context.endpoint, true, false
}

// NetHolder reports the handles a family's session lives in.
func NetHolder(familyID uint64) (NetGrant, bool) {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	context, _, found := netContextHeld(familyID)
	if !found || !context.attached {
		return NetGrant{}, false
	}
	return NetGrant{Holder: context.endpoint.Holder, ChannelHandle: context.channelGrant, PoolHandle: context.poolGrant,
		Channel: context.endpoint.Channel, Side: context.endpoint.Side, PoolObject: context.endpoint.PoolObject}, true
}

// FailNet marks a family's session failed: every socket of it fails and no
// new exchange starts. It reports whether this call failed it, so the
// caller wakes the session's waiters once, and returns the waiters for a
// request slot, who must wake to see the failure.
func FailNet(familyID uint64, session uint32, waiters []sched.TaskID) (NetEndpoint, int, bool) {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	context, _, found := netContextHeld(familyID)
	if !found || !context.attached || context.failed || context.endpoint.Session != session {
		return NetEndpoint{}, 0, false
	}
	context.failed = true
	noteNetHeld(&netDiagnostics.SessionFailures)
	return context.endpoint, context.takeWaitersHeld(waiters), true
}

// NewSocket takes a slot in the family's socket table for a socket the
// service opened or accepted and returns the description object for it.
func NewSocket(familyID uint64, session uint32, identity uint64, datagram bool) (uint64, bool) {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	context, contextIndex, found := netContextHeld(familyID)
	if !found || !context.attached || context.failed || context.endpoint.Session != session || identity == 0 {
		return 0, false
	}
	for slot := 0; slot < MaxSockets; slot++ {
		socket := &context.sockets[slot]
		if socket.used {
			continue
		}
		generation := socket.generation + 1
		if generation&0xffffff == 0 {
			generation = 1
		}
		*socket = NetSocket{Identity: identity, Session: session, Datagram: datagram, generation: generation, used: true}
		noteNetHeld(&netDiagnostics.Opens)
		return socketObject(contextIndex, slot, generation), true
	}
	return 0, false
}

// HasSocketRoom reports whether NewSocket would find a slot.
func HasSocketRoom(familyID uint64) bool {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	context, _, found := netContextHeld(familyID)
	if !found {
		return false
	}
	for slot := 0; slot < MaxSockets; slot++ {
		if !context.sockets[slot].used {
			return true
		}
	}
	return false
}

func socketHeld(object uint64) (*NetContext, *NetSocket, bool) {
	guard.Check()
	contextIndex, slot, generation, ok := splitSocketObject(object)
	if !ok || contextIndex < 0 || contextIndex >= len(netContexts) || slot < 0 || slot >= MaxSockets {
		return nil, nil, false
	}
	context := &netContexts[contextIndex]
	socket := &context.sockets[slot]
	if context.familyID == 0 || !socket.used || socket.generation != generation {
		return nil, nil, false
	}
	return context, socket, true
}

// SocketOf returns the socket a description names. live reports that it
// belongs to the family's current session, which has not failed.
func SocketOf(object uint64) (socket NetSocket, familyID uint64, live bool, found bool) {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	context, entry, ok := socketHeld(object)
	if !ok {
		return NetSocket{}, 0, false, false
	}
	return *entry, context.familyID, context.attached && !context.failed && entry.Session == context.endpoint.Session, true
}

// StoreSocket writes back the Linux state of the socket a description
// names, when it is still there. Its identity and session stay.
func StoreSocket(object uint64, socket NetSocket) bool {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	_, entry, ok := socketHeld(object)
	if !ok {
		return false
	}
	entry.Listening, entry.ReuseAddr, entry.ShutRead, entry.ShutWrite = socket.Listening, socket.ReuseAddr, socket.ShutRead, socket.ShutWrite
	entry.Connecting, entry.RecvTimeout, entry.SendTimeout = socket.Connecting, socket.RecvTimeout, socket.SendTimeout
	return true
}

// CloseSocket forgets the socket of a description whose last reference
// went and reports the identity the service must close, when its session is
// the live one. A socket of a dead session needs nothing sent.
func CloseSocket(object uint64) (identity uint64, familyID uint64, send bool) {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	context, entry, ok := socketHeld(object)
	if !ok {
		return 0, 0, false
	}
	identity, familyID = entry.Identity, context.familyID
	send = context.attached && !context.failed && entry.Session == context.endpoint.Session
	generation := entry.generation
	*entry = NetSocket{generation: generation}
	noteNetHeld(&netDiagnostics.Closes)
	return identity, familyID, send
}

// RequestStatus is the outcome of BeginRequest.
type RequestStatus uint8

const (
	RequestStarted RequestStatus = iota + 1
	// RequestBusy means every request slot is in use; the caller waits
	// for one.
	RequestBusy
	// RequestFailed means the session is gone or never existed.
	RequestFailed
)

// BeginRequest reserves a request slot and a correlation for one request of
// task, owner zero for a request nobody waits for. The slot's index is the
// TX slot a Write or SendTo uses. A busy table queues task to be woken when
// a slot frees, unless queue is false.
func BeginRequest(familyID uint64, session uint32, task sched.TaskID, message *linuxsockapi.Request, deadline uint64, queue bool) (index int, correlation uint64, status RequestStatus) {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	context, _, found := netContextHeld(familyID)
	if !found || !context.attached || context.failed || context.endpoint.Session != session {
		return 0, 0, RequestFailed
	}
	for index = 0; index < MaxSocketRequests; index++ {
		request := &context.requests[index]
		if request.state != requestFree {
			continue
		}
		context.correlation++
		if context.correlation < firstSocketCorrelation {
			context.correlation = firstSocketCorrelation
		}
		state := requestSent
		if task == 0 {
			state = requestAbandoned
		}
		request.correlation, request.task, request.socket, request.deadline = context.correlation, task, message.Socket, deadline
		request.opcode, request.flags, request.state, request.reply = message.Opcode, message.Flags, state, linuxsockapi.Reply{}
		return index, context.correlation, RequestStarted
	}
	if queue && task != 0 {
		context.addWaiterHeld(task)
	}
	return 0, 0, RequestBusy
}

func (context *NetContext) addWaiterHeld(task sched.TaskID) {
	for index := 0; index < int(context.waiterCount) && index < len(context.waiters); index++ {
		if context.waiters[index] == task {
			return
		}
	}
	if int(context.waiterCount) < len(context.waiters) {
		context.waiters[context.waiterCount] = task
		context.waiterCount++
	}
}

func (context *NetContext) takeWaitersHeld(target []sched.TaskID) int {
	// Waiters beyond target stay queued for the next slot that frees.
	count := min(int(context.waiterCount), len(target), len(context.waiters))
	copy(target, context.waiters[:count])
	remaining := int(context.waiterCount) - count
	copy(context.waiters[:], context.waiters[count:int(context.waiterCount)])
	clear(context.waiters[remaining:])
	context.waiterCount = uint8(remaining)
	return count
}

// RemoveSocketWaiter takes a task that gives up out of the queue for a
// request slot.
func RemoveSocketWaiter(task sched.TaskID) {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	for contextIndex := 0; contextIndex < len(netContexts); contextIndex++ {
		context := &netContexts[contextIndex]
		for index := 0; index < int(context.waiterCount) && index < len(context.waiters); index++ {
			if context.waiters[index] != task {
				continue
			}
			copy(context.waiters[index:], context.waiters[index+1:int(context.waiterCount)])
			context.waiterCount--
			context.waiters[context.waiterCount] = 0
			return
		}
	}
}

func (context *NetContext) requestHeld(correlation uint64) (*socketRequest, bool) {
	if correlation < firstSocketCorrelation {
		return nil, false
	}
	for index := 0; index < MaxSocketRequests; index++ {
		request := &context.requests[index]
		if request.state != requestFree && request.correlation == correlation {
			return request, true
		}
	}
	return nil, false
}

// freeHeld ends a request and hands the queued waiters to the caller to wake.
func (context *NetContext) freeHeld(request *socketRequest, waiters []sched.TaskID) int {
	*request = socketRequest{}
	return context.takeWaitersHeld(waiters)
}

// CancelRequest ends a request that was never queued.
func CancelRequest(familyID uint64, correlation uint64, waiters []sched.TaskID) int {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	context, _, found := netContextHeld(familyID)
	if !found {
		return 0
	}
	request, ok := context.requestHeld(correlation)
	if !ok {
		return 0
	}
	return context.freeHeld(request, waiters)
}

// SocketCleanup is what a reply nobody takes leaves to do: return an RX
// slot, and close a socket the service opened or accepted for nobody.
type SocketCleanup struct {
	Release      bool
	ReleaseSlot  uint8
	ReleaseOwner uint64
	ReleaseBytes uint32
	Close        uint64
}

// MessageVerdict classifies one message from the session channel.
type MessageVerdict uint8

const (
	// MessageReadyChanged is the service's notification.
	MessageReadyChanged MessageVerdict = iota + 1
	// MessageReply answers a request; Owner and Correlation name it.
	MessageReply
	// MessageMalformed fails the session.
	MessageMalformed
)

// ClassifySocketMessage checks one message against the session's requests.
// A reply must answer a request in flight with the request's opcode and
// socket, use the slots the protocol gives it, and name no socket the
// family already holds as a new one. It changes nothing; DeliverReply or
// AbandonReply applies the reply.
func ClassifySocketMessage(familyID uint64, session uint32, message *ipc.Message, reply *linuxsockapi.Reply) (verdict MessageVerdict, owner sched.TaskID, correlation uint64) {
	if message.HandleCount != 0 {
		return MessageMalformed, 0, 0
	}
	if message.Kind == ipc.KindNotification {
		var notification linuxsockapi.Request
		if _, ok := linuxsockapi.DecodeRequest(message, &notification); ok && notification.Opcode == linuxsockapi.OpReadyChanged {
			return MessageReadyChanged, 0, 0
		}
		return MessageMalformed, 0, 0
	}
	correlation, ok := linuxsockapi.DecodeReply(message, reply)
	if !ok {
		return MessageMalformed, 0, 0
	}
	tablesLock.Lock()
	defer tablesLock.Unlock()
	context, _, found := netContextHeld(familyID)
	if !found || !context.attached || context.endpoint.Session != session {
		return MessageMalformed, 0, 0
	}
	request, ok := context.requestHeld(correlation)
	if !ok || request.state != requestSent && request.state != requestAbandoned || reply.Opcode != request.opcode {
		return MessageMalformed, 0, 0
	}
	switch reply.Opcode {
	case linuxsockapi.OpOpen:
		if reply.Result == linuxsockapi.ResultOK && context.knownIdentityHeld(reply.Socket) {
			return MessageMalformed, 0, 0
		}
	case linuxsockapi.OpCancel:
	default:
		if reply.Socket != request.socket {
			return MessageMalformed, 0, 0
		}
	}
	switch reply.Opcode {
	case linuxsockapi.OpAccept:
		if reply.Result == linuxsockapi.ResultOK && (reply.Value == reply.Socket || context.knownIdentityHeld(reply.Value)) {
			return MessageMalformed, 0, 0
		}
	case linuxsockapi.OpRead:
		if reply.Result == linuxsockapi.ResultOK && reply.Length != 0 && request.flags&linuxsockapi.FlagDiscard == 0 && (int(reply.Slot) >= len(context.rx) || context.rx[reply.Slot].held) {
			return MessageMalformed, 0, 0
		}
	case linuxsockapi.OpWrite, linuxsockapi.OpSendTo:
		if int(reply.Slot) != requestIndex(context, request) {
			return MessageMalformed, 0, 0
		}
	}
	if request.state == requestAbandoned {
		return MessageReply, 0, correlation
	}
	return MessageReply, request.task, correlation
}

func requestIndex(context *NetContext, request *socketRequest) int {
	for index := 0; index < MaxSocketRequests; index++ {
		if &context.requests[index] == request {
			return index
		}
	}
	return -1
}

func (context *NetContext) knownIdentityHeld(identity uint64) bool {
	for slot := 0; slot < MaxSockets; slot++ {
		if context.sockets[slot].used && context.sockets[slot].Identity == identity && context.sockets[slot].Session == context.endpoint.Session {
			return true
		}
	}
	return false
}

// DeliverReply stores a classified reply for its owner, which takes it with
// TakeReply. A Read reply's RX slot is the kernel's from here on.
func DeliverReply(familyID uint64, correlation uint64, reply *linuxsockapi.Reply) bool {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	context, _, found := netContextHeld(familyID)
	if !found {
		return false
	}
	request, ok := context.requestHeld(correlation)
	if !ok || request.state != requestSent {
		return false
	}
	if request.flags&linuxsockapi.FlagDiscard == 0 {
		context.holdReplySlotHeld(reply)
	}
	request.reply, request.state = *reply, requestReplied
	return true
}

func (context *NetContext) holdReplySlotHeld(reply *linuxsockapi.Reply) {
	if reply.Opcode == linuxsockapi.OpRead && reply.Result == linuxsockapi.ResultOK && reply.Length != 0 && int(reply.Slot) < len(context.rx) {
		context.rx[reply.Slot] = heldSlot{socket: reply.Socket, held: true}
	}
}

// AbandonReply applies a classified reply whose owner is gone and frees its
// request. cleanup says what to send: the reply's RX bytes go back with
// Release, which makes the service return them first, and a socket the reply
// created for nobody is closed.
func AbandonReply(familyID uint64, correlation uint64, reply *linuxsockapi.Reply, waiters []sched.TaskID) (cleanup SocketCleanup, woken int) {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	context, _, found := netContextHeld(familyID)
	if !found {
		return SocketCleanup{}, 0
	}
	request, ok := context.requestHeld(correlation)
	if !ok {
		return SocketCleanup{}, 0
	}
	cleanup = abandonedCleanup(reply, request.flags)
	if cleanup.Release && int(cleanup.ReleaseSlot) < len(context.rx) {
		context.rx[cleanup.ReleaseSlot] = heldSlot{socket: reply.Socket, length: reply.Length, held: true, unsent: true}
	}
	noteNetHeld(&netDiagnostics.Abandoned)
	return cleanup, context.freeHeld(request, waiters)
}

// A Discard read consumes without copying and lends no slot.
func abandonedCleanup(reply *linuxsockapi.Reply, flags uint8) SocketCleanup {
	if reply.Result != linuxsockapi.ResultOK {
		return SocketCleanup{}
	}
	switch reply.Opcode {
	case linuxsockapi.OpOpen:
		return SocketCleanup{Close: reply.Socket}
	case linuxsockapi.OpAccept:
		return SocketCleanup{Close: reply.Value}
	case linuxsockapi.OpRead:
		if reply.Length != 0 && flags&linuxsockapi.FlagDiscard == 0 {
			return SocketCleanup{Release: true, ReleaseSlot: reply.Slot, ReleaseOwner: reply.Socket, ReleaseBytes: reply.Length}
		}
	}
	return SocketCleanup{}
}

// ReplyWait is what TakeReply finds for a request.
type ReplyWait uint8

const (
	ReplyTaken ReplyWait = iota + 1
	// ReplyPending is still in flight; the owner waits until Deadline.
	ReplyPending
	// ReplyGone names no request of the live session.
	ReplyGone
)

// TakeReply hands a request's reply to its owner and frees the request.
func TakeReply(familyID uint64, session uint32, correlation uint64, reply *linuxsockapi.Reply, waiters []sched.TaskID) (wait ReplyWait, deadline uint64, woken int) {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	context, _, found := netContextHeld(familyID)
	if !found || !context.attached || context.failed || context.endpoint.Session != session {
		return ReplyGone, 0, 0
	}
	request, ok := context.requestHeld(correlation)
	if !ok || request.state == requestAbandoned {
		return ReplyGone, 0, 0
	}
	if request.state == requestSent {
		return ReplyPending, request.deadline, 0
	}
	*reply = request.reply
	return ReplyTaken, 0, context.freeHeld(request, waiters)
}

// AbandonRequest gives up waiting for a request: its reply, now or later,
// is applied as one nobody takes.
func AbandonRequest(familyID uint64, correlation uint64, waiters []sched.TaskID) (cleanup SocketCleanup, woken int) {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	context, _, found := netContextHeld(familyID)
	if !found {
		return SocketCleanup{}, 0
	}
	request, ok := context.requestHeld(correlation)
	if !ok {
		return SocketCleanup{}, 0
	}
	if request.state == requestSent {
		request.state, request.task = requestAbandoned, 0
		noteNetHeld(&netDiagnostics.Abandoned)
		return SocketCleanup{}, 0
	}
	if request.state != requestReplied {
		return SocketCleanup{}, 0
	}
	cleanup = abandonedCleanup(&request.reply, request.flags)
	if cleanup.Release && int(cleanup.ReleaseSlot) < len(context.rx) {
		context.rx[cleanup.ReleaseSlot] = heldSlot{socket: request.reply.Socket, length: request.reply.Length, held: true, unsent: true}
	}
	noteNetHeld(&netDiagnostics.Abandoned)
	return cleanup, context.freeHeld(request, waiters)
}

// AbandonTask gives up every request task has in flight in any family, for
// a task that ends. Their replies are applied as ones nobody takes.
func AbandonTask(task sched.TaskID) {
	if task == 0 {
		return
	}
	tablesLock.Lock()
	defer tablesLock.Unlock()
	for contextIndex := 0; contextIndex < len(netContexts); contextIndex++ {
		context := &netContexts[contextIndex]
		for index := 0; index < MaxSocketRequests; index++ {
			request := &context.requests[index]
			if request.task == task && request.state == requestSent {
				request.state, request.task = requestAbandoned, 0
			}
		}
	}
}

// ReleaseSlot records how much of a held RX slot the kernel left unread.
// The caller sends the Release and confirms it with SlotReleased; one that
// cannot be queued stays recorded for the next flush.
func ReleaseSlot(familyID uint64, slot uint8, unread uint32, offset uint32) bool {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	context, _, found := netContextHeld(familyID)
	if !found || int(slot) >= len(context.rx) || !context.rx[slot].held {
		return false
	}
	held := &context.rx[slot]
	held.length, held.offset, held.unsent = unread, offset, true
	return true
}

// SlotReleased ends the kernel's hold of an RX slot whose Release is queued.
func SlotReleased(familyID uint64, slot uint8) {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	if context, _, found := netContextHeld(familyID); found && int(slot) < len(context.rx) {
		context.rx[slot] = heldSlot{}
	}
}

// PendingRelease returns the first RX slot whose Release is still to send.
func PendingRelease(familyID uint64) (request linuxsockapi.Request, slot uint8, found bool) {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	context, _, ok := netContextHeld(familyID)
	if !ok {
		return linuxsockapi.Request{}, 0, false
	}
	for index := 0; index < len(context.rx); index++ {
		held := &context.rx[index]
		if held.held && held.unsent {
			return linuxsockapi.Request{Opcode: linuxsockapi.OpRelease, Socket: held.socket, Slot: uint8(index),
				Length: held.length, Value: uint64(held.offset)}, uint8(index), true
		}
	}
	return linuxsockapi.Request{}, 0, false
}

// DeferClose records a Close the kernel could not send now.
func DeferClose(familyID uint64, identity uint64) bool {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	context, _, found := netContextHeld(familyID)
	if !found || identity == 0 || !context.attached || context.failed || int(context.closeCount) >= len(context.closes) {
		return false
	}
	context.closes[context.closeCount] = identity
	context.closeCount++
	return true
}

// TakeDeferredClose removes one deferred Close for sending.
func TakeDeferredClose(familyID uint64) (uint64, bool) {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	context, _, found := netContextHeld(familyID)
	if !found || context.closeCount == 0 || int(context.closeCount) > len(context.closes) {
		return 0, false
	}
	context.closeCount--
	identity := context.closes[context.closeCount]
	context.closes[context.closeCount] = 0
	return identity, true
}

// NetDiagnostics counts Linux socket traffic. The counters saturate; no
// payload is ever recorded.
type NetDiagnostics struct {
	Attaches        uint64
	Opens           uint64
	Closes          uint64
	Connects        uint64
	Accepts         uint64
	BytesSent       uint64
	BytesReceived   uint64
	DatagramsSent   uint64
	DatagramsRecv   uint64
	WouldBlock      uint64
	PollWakeups     uint64
	Interrupted     uint64
	SessionFailures uint64
	Abandoned       uint64
	Requests        uint64
	ReplyTimeouts   uint64
}

var netDiagnostics NetDiagnostics

// noteNetHeld counts one event of a counter the tables' steps keep.
func noteNetHeld(counter *uint64) {
	diagnosticsLock.Lock()
	addNet(counter, 1)
	diagnosticsLock.Unlock()
}

func addNet(counter *uint64, amount uint64) {
	if ^uint64(0)-*counter < amount {
		*counter = ^uint64(0)
		return
	}
	*counter += amount
}

// NetCounter names one counter for NoteNet.
type NetCounter uint8

const (
	NetConnects NetCounter = iota + 1
	NetAccepts
	NetBytesSent
	NetBytesReceived
	NetDatagramsSent
	NetDatagramsReceived
	NetWouldBlock
	NetPollWakeups
	NetInterrupted
	NetRequests
	NetReplyTimeouts
)

// NoteNet adds amount to one counter.
func NoteNet(counter NetCounter, amount uint64) {
	diagnosticsLock.Lock()
	target := (*uint64)(nil)
	switch counter {
	case NetConnects:
		target = &netDiagnostics.Connects
	case NetAccepts:
		target = &netDiagnostics.Accepts
	case NetBytesSent:
		target = &netDiagnostics.BytesSent
	case NetBytesReceived:
		target = &netDiagnostics.BytesReceived
	case NetDatagramsSent:
		target = &netDiagnostics.DatagramsSent
	case NetDatagramsReceived:
		target = &netDiagnostics.DatagramsRecv
	case NetWouldBlock:
		target = &netDiagnostics.WouldBlock
	case NetPollWakeups:
		target = &netDiagnostics.PollWakeups
	case NetInterrupted:
		target = &netDiagnostics.Interrupted
	case NetRequests:
		target = &netDiagnostics.Requests
	case NetReplyTimeouts:
		target = &netDiagnostics.ReplyTimeouts
	}
	if target != nil {
		addNet(target, amount)
	}
	diagnosticsLock.Unlock()
}

// NetStatistics returns a copy of the counters.
func NetStatistics() NetDiagnostics {
	diagnosticsLock.Lock()
	result := netDiagnostics
	diagnosticsLock.Unlock()
	return result
}

// SocketRequestCorrelation reports whether a continuation's correlation
// names a request in flight rather than a readiness sleep.
func SocketRequestCorrelation(correlation uint64) bool { return correlation >= firstSocketCorrelation }

// RequestSlotFree reports whether BeginRequest would find a request slot.
func RequestSlotFree(familyID uint64) bool {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	context, _, found := netContextHeld(familyID)
	if !found {
		return false
	}
	for index := 0; index < MaxSocketRequests; index++ {
		if context.requests[index].state == requestFree {
			return true
		}
	}
	return false
}

// RequestDeadline returns the reply deadline of a request its owner still
// waits for, or zero.
func RequestDeadline(familyID uint64, session uint32, correlation uint64) uint64 {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	context, _, found := netContextHeld(familyID)
	if !found || !context.attached || context.failed || context.endpoint.Session != session {
		return 0
	}
	request, ok := context.requestHeld(correlation)
	if !ok || request.state != requestSent && request.state != requestReplied {
		return 0
	}
	return request.deadline
}
