//go:build goos_userspace

package main

import (
	"goos/internal/capability"
	"goos/internal/validation"
	"goos/kernel/clock"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	configproto "goos/userspace/config"
	execproto "goos/userspace/exec"
	supervisor "goos/userspace/init"
	syslog "goos/userspace/log"
	"goos/userspace/remoteapi"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/sys"
	"goos/userspace/startup"
	userwait "goos/userspace/wait"
)

const remoteServiceName = "remoted"

// Worker slots are not supervisor services. A slot is reused only after its
// shell, jobs, processes, mappings, streams, and endpoints have been released.
type remoteWorker struct {
	id, owner             uint64
	process               handle.ID
	pid                   uint64
	control               handle.ID
	input, output, policy handle.ID
	limit                 uint8
	session               terminalSession
	tty                   handle.ID
	fill                  pendingFill
	opening               bool
	requestCorrelation    uint64
	grace                 supervisor.RemoteGrace
}

var (
	remoteRegistry       supervisor.RemoteRegistry
	remoteWorkers        [remoteapi.MaxWorkers]remoteWorker
	remoteOwner          uint64
	remoteControl        handle.ID
	remoteProcess        handle.ID
	remoteCountsPending  bool
	remoteCountsDeadline uint64
)

func remoteSession(session *terminalSession) *remoteWorker {
	for index := range remoteWorkers {
		worker := &remoteWorkers[index]
		if worker.id != 0 && &worker.session == session {
			return worker
		}
	}
	return nil
}

func startRemote(ticks uint64) {
	if boot.Mode() != supervisor.ModeNormal {
		return
	}
	if _, found := manager.Snapshot(netServiceName); !found {
		return
	}
	if manager.Register(supervisor.Definition{Name: remoteServiceName, Executable: supervisor.ServicePath(remoteServiceName, boot.Mode()),
		Dependencies: []string{"vfs", "config", netServiceName}, Restart: supervisor.RestartAlways,
		Capabilities: []supervisor.Capability{{Name: "log", Handle: logDeviceHandle, Type: handle.TypeDevice, Rights: capability.Write}}}) != supervisor.StatusOK {
		return
	}
	manager.StartReady(ticks+supervisor.RestartBackoff, launchService)
}

// Each listener launch receives new brokers and a control endpoint. Only the
// listener has this endpoint; workers get independently created channels.
func prepareRemoteGrants(spec *supervisor.LaunchSpec) bool {
	closeRemoteWorkers()
	closeOpen(remoteControl)
	remoteControl = 0
	var control [2]uint64
	if !createChannel(&control) {
		return false
	}
	files, reason := openRemoteAuthority()
	if reason != "" {
		closePair(control)
		return false
	}
	settings, ok := openConfigSession("system.remote.", configproto.Read)
	if !ok {
		closePair(control)
		closeOpen(files)
		return false
	}
	tcp, ok := openTCPSession()
	if !ok {
		closePair(control)
		closeOpen(files)
		closeOpen(settings)
		return false
	}
	grants := []supervisor.Capability{
		{Name: "control", Handle: handle.ID(control[1]), Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
		{Name: "vfs", Handle: files, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
		{Name: "config", Handle: settings, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
		{Name: "tcp", Handle: tcp[0], Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
		{Name: "tcp-tx", Handle: tcp[1], Type: handle.TypeMemory, Rights: capability.Read | capability.Write, Consume: true},
		{Name: "tcp-rx", Handle: tcp[2], Type: handle.TypeMemory, Rights: capability.Read, Consume: true},
	}
	for _, grant := range grants {
		spec.Capabilities[spec.CapabilityCount] = grant
		spec.CapabilityCount++
	}
	remoteControl = handle.ID(control[0])
	return true
}

func abortRemoteGrants(spec *supervisor.LaunchSpec) {
	for index := 1; index < int(spec.CapabilityCount); index++ {
		closeOpen(spec.Capabilities[index].Handle)
	}
	closeOpen(remoteControl)
	remoteControl = 0
}

func pollRemoteListener() {
	arena.Reset(arenaBase)
	message, status := receiveNow(remoteControl)
	if status == ipc.StatusNotFound || status == ipc.StatusTimeout {
		return
	}
	if status != ipc.StatusOK {
		closeRemoteWorkers()
		closeOpen(remoteControl)
		remoteControl = 0
		return
	}
	var request remoteapi.WorkerRequest
	if !remoteapi.DecodeWorkerRequest(&message, &request) {
		closeMessageHandles(&message)
		return
	}
	if request.Operation == remoteapi.WorkerAuthority {
		remoteAuthority(&request, message.Correlation)
		return
	}
	if request.Operation == remoteapi.WorkerClose {
		for index := range remoteWorkers {
			if remoteWorkers[index].id == request.Worker {
				closeRemoteWorker(&remoteWorkers[index])
				replyWorker(request.Operation, request.Worker, remoteapi.ResultOK, message.Correlation)
				return
			}
		}
		replyWorker(request.Operation, request.Worker, remoteapi.ResultNotFound, message.Correlation)
		return
	}
	worker := freeRemoteWorker()
	if worker == nil || remoteTasks()+1 > supervisor.RemoteTaskLimit {
		closeMessageHandles(&message)
		replyWorker(request.Operation, 0, remoteapi.ResultLimit, message.Correlation)
		return
	}
	policy, limit, valid := copyRemotePolicy(message.Handles[0])
	closeOpen(message.Handles[0])
	message.Handles[0] = 0
	if !valid {
		closeMessageHandles(&message)
		replyWorker(request.Operation, 0, remoteapi.ResultInvalidArgument, message.Correlation)
		return
	}
	if remoteOwner == ^uint64(0) {
		closeOpen(policy)
		closeMessageHandles(&message)
		replyWorker(request.Operation, 0, remoteapi.ResultLimit, message.Correlation)
		return
	}
	remoteOwner++
	id, result := remoteRegistry.Admit(remoteOwner, uint64(policy))
	if result != remoteapi.ResultOK {
		closeOpen(policy)
		closeMessageHandles(&message)
		replyWorker(request.Operation, 0, result, message.Correlation)
		return
	}
	*worker = remoteWorker{id: id, owner: remoteOwner, policy: policy, limit: limit, requestCorrelation: message.Correlation}
	notifyRemoteCounts()
	if !beginRemoteImage(worker, []byte(supervisor.ServicePath("remoted-worker", boot.Mode()))) {
		failRemoteOpen(worker, remoteapi.ResultUnavailable)
	}
}

func freeRemoteWorker() *remoteWorker {
	for index := range remoteWorkers {
		if remoteWorkers[index].id == 0 {
			return &remoteWorkers[index]
		}
	}
	return nil
}

func replyWorker(operation remoteapi.WorkerOperation, id uint64, result remoteapi.Result, requestCorrelation uint64) bool {
	var message ipc.Message
	return remoteControl != 0 && remoteapi.EncodeWorkerReply(&remoteapi.WorkerReply{Operation: operation, Worker: id, Result: result}, requestCorrelation, &message) && sendMessage(uint64(remoteControl), message)
}

func closeMessageHandles(message *ipc.Message) {
	for index := 0; index < int(message.HandleCount) && index < len(message.Handles); index++ {
		closeOpen(message.Handles[index])
		message.Handles[index] = 0
	}
}

func beginRemoteImage(worker *remoteWorker, path []byte) bool {
	if worker.fill.session != 0 {
		return false
	}
	fill, status := beginSource(path)
	if status != supervisor.LaunchOK {
		return false
	}
	worker.fill = fill
	return true
}

func completeRemoteFill(worker *remoteWorker) {
	arena.Reset(arenaBase)
	pending := worker.fill
	worker.fill = pendingFill{}
	message, status := receiveNow(pending.session)
	executable, result := finishSource(pending, message, status)
	if result != supervisor.LaunchOK {
		syslog.Warn("remote executable preparation failed", syslog.Uint("status", uint64(result)), syslog.Uint("session", worker.id))
		failRemoteOpen(worker, remoteapi.ResultUnavailable)
		return
	}
	if worker.opening {
		launchRemoteShell(worker, executable)
	} else {
		launchRemoteWorker(worker, executable)
	}
	closeExecutable(executable)
}

func launchRemoteWorker(worker *remoteWorker, executable supervisor.Executable) {
	var pair [2]uint64
	if !createChannel(&pair) {
		failRemoteOpen(worker, remoteapi.ResultUnavailable)
		return
	}
	worker.control = handle.ID(pair[0])
	defer closeOpen(handle.ID(pair[1]))
	var signer [2]uint64
	if !createChannel(&signer) {
		failRemoteOpen(worker, remoteapi.ResultUnavailable)
		return
	}
	defer closeOpen(handle.ID(signer[0]))
	defer closeOpen(handle.ID(signer[1]))
	var input, output [2]uint64
	if !createStream(&input) {
		failRemoteOpen(worker, remoteapi.ResultUnavailable)
		return
	}
	if !createStream(&output) {
		closePair(input)
		failRemoteOpen(worker, remoteapi.ResultUnavailable)
		return
	}
	worker.input, worker.output = handle.ID(input[0]), handle.ID(output[1])
	defer closeOpen(handle.ID(input[1]))
	defer closeOpen(handle.ID(output[0]))
	grants := []supervisor.Capability{
		{Name: "control", Handle: handle.ID(pair[1]), Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
		{Name: "input", Handle: worker.input, Type: handle.TypeStream, Rights: capability.Read | capability.Wait, Consume: true},
		{Name: "output", Handle: worker.output, Type: handle.TypeStream, Rights: capability.Write | capability.Wait, Consume: true},
		{Name: "policy", Handle: worker.policy, Type: handle.TypeMemory, Rights: capability.Read, Consume: true},
		{Name: "signer", Handle: handle.ID(signer[1]), Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
		{Name: "log", Handle: logDeviceHandle, Type: handle.TypeDevice, Rights: capability.Write},
	}
	event, result := launchRemoteExecutable(executable, "remoted-worker", grants, false)
	if result != supervisor.LaunchOK {
		closeOpen(handle.ID(pair[1]))
		failRemoteOpen(worker, remoteapi.ResultUnavailable)
		return
	}
	worker.process, worker.pid = event.ProcessHandle, event.Process
	worker.input, worker.output, worker.policy = 0, 0, 0
	var reply ipc.Message
	if !remoteapi.EncodeWorkerLaunchReply(&remoteapi.WorkerReply{Operation: remoteapi.WorkerLaunch, Worker: worker.id, Result: remoteapi.ResultOK}, []handle.ID{handle.ID(input[1]), handle.ID(output[0]), handle.ID(signer[0])}, worker.requestCorrelation, &reply) || !sendMessage(uint64(remoteControl), reply) {
		closeRemoteWorker(worker)
	}
	worker.requestCorrelation = 0
	notifyRemoteCounts()
}

func launchRemoteExecutable(executable supervisor.Executable, name string, grants []supervisor.Capability, shell bool) (supervisor.LifecycleEvent, supervisor.LaunchStatus) {
	slot, staging := stagingSlot()
	variables := 0
	if shell {
		variables = 2
	}
	writer := startup.NewWriter(staging, 1, variables, len(grants))
	writer.Command([]byte(name))
	writer.CWD([]byte("/"))
	writer.Argument([]byte(name))
	if shell {
		writer.Variable([]byte("PATH"), []byte(supervisor.SearchPath(boot.Mode())))
		writer.Variable([]byte("GOOS_SESSION"), []byte("remote"))
	}
	for _, grant := range grants {
		writer.Handle([]byte(grant.Name), 0)
	}
	data, ok := writer.Finish()
	if !ok {
		return supervisor.LifecycleEvent{}, supervisor.LaunchArgumentsTooLong
	}
	return requestLaunch(executable, name, grants, supervisor.ProfileNormal, slot, uint32(len(data)))
}

func pollRemoteWorker(worker *remoteWorker) {
	arena.Reset(arenaBase)
	message, status := receiveNow(worker.control)
	if status == ipc.StatusNotFound || status == ipc.StatusTimeout {
		return
	}
	if status != ipc.StatusOK {
		closeRemoteWorker(worker)
		return
	}
	var request remoteapi.Request
	if !remoteapi.DecodeRequest(&message, &request) {
		closeMessageHandles(&message)
		closeRemoteWorker(worker)
		return
	}
	result := remoteapi.ResultInvalidArgument
	switch request.Operation {
	case remoteapi.OperationAuthenticate:
		result = remoteRegistry.Authenticate(worker.id, worker.owner, int(worker.limit))
		if result == remoteapi.ResultOK {
			notifyRemoteCounts()
		}
	case remoteapi.OperationOpen:
		if remoteTasks()+1 > supervisor.RemoteTaskLimit {
			result = remoteapi.ResultLimit
			break
		}
		result = remoteRegistry.BeginOpen(worker.id, worker.owner, request.Geometry)
		if result == remoteapi.ResultOK {
			worker.opening = true
			worker.requestCorrelation = message.Correlation
			if !beginRemoteImage(worker, []byte(supervisor.ServicePath("shell", boot.Mode()))) {
				failRemoteOpen(worker, remoteapi.ResultUnavailable)
			}
			return
		}
	case remoteapi.OperationGeometry:
		if request.Session != worker.id {
			result = remoteapi.ResultNotFound
			break
		}
		result = remoteRegistry.Resize(worker.id, worker.owner, request.Geometry)
	case remoteapi.OperationClose:
		if request.Session != worker.id {
			result = remoteapi.ResultNotFound
			break
		}
		replyRemote(worker, request.Operation, remoteapi.ResultOK, nil, message.Correlation)
		closeRemoteWorker(worker)
		return
	}
	if !replyRemote(worker, request.Operation, result, nil, message.Correlation) {
		closeRemoteWorker(worker)
	}
}

func replyRemote(worker *remoteWorker, operation remoteapi.Operation, result remoteapi.Result, attachments []handle.ID, requestCorrelation uint64) bool {
	var reply ipc.Message
	return remoteapi.EncodeReply(&remoteapi.Reply{Operation: operation, Result: result, Session: worker.id}, attachments, requestCorrelation, &reply) && sendMessage(uint64(worker.control), reply)
}

func launchRemoteShell(worker *remoteWorker, executable supervisor.Executable) {
	session := &worker.session
	var tty, exec [2]uint64
	if !createStream(&session.output) || !createChannel(&tty) || !createChannel(&exec) {
		syslog.Warn("remote shell channel allocation failed", syslog.Uint("session", worker.id))
		closePair(tty)
		closePair(exec)
		failRemoteOpen(worker, remoteapi.ResultUnavailable)
		return
	}
	worker.tty = handle.ID(tty[1])
	session.execClient, session.execServer = handle.ID(exec[0]), exec[1]
	page, status := sys.MemoryCreate(execproto.AreaSize / 4096)
	if status != kcall.StatusOK {
		syslog.Warn("remote shell page allocation failed", syslog.Uint("status", uint64(status)), syslog.Uint("session", worker.id))
		closeOpen(handle.ID(tty[0]))
		failRemoteOpen(worker, remoteapi.ResultUnavailable)
		return
	}
	session.pageHandle = page
	base, size, status := sys.MemoryMapAnywhere(page, true)
	if status != kcall.StatusOK || size < execproto.AreaSize {
		syslog.Warn("remote shell page mapping failed", syslog.Uint("status", uint64(status)), syslog.Uint("session", worker.id))
		if status == kcall.StatusOK {
			sys.VMUnmap(base, size)
		}
		closeOpen(handle.ID(tty[0]))
		failRemoteOpen(worker, remoteapi.ResultUnavailable)
		return
	}
	session.page = sys.Window(base, execproto.AreaSize)
	session.pageBase, session.pageSize = base, size
	files, reason := openVFSSession()
	if reason != "" {
		syslog.Warn("remote shell filesystem session failed", syslog.Str("reason", reason), syslog.Uint("session", worker.id))
		closeOpen(handle.ID(tty[0]))
		failRemoteOpen(worker, remoteapi.ResultUnavailable)
		return
	}
	settings, _ := openConfigSession("", configproto.Read|configproto.Write|configproto.HostsRead)
	grants := []supervisor.Capability{
		{Name: "stdout", Handle: handle.ID(session.output[1]), Type: handle.TypeStream, Rights: capability.Write | capability.Wait},
		{Name: "log", Handle: logDeviceHandle, Type: handle.TypeDevice, Rights: capability.Read},
		{Name: "tty", Handle: handle.ID(tty[0]), Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
		{Name: "vfs", Handle: files, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
		{Name: "exec", Handle: session.execClient, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
		{Name: "execpage", Handle: page, Type: handle.TypeMemory, Rights: capability.Read | capability.Write},
	}
	if settings != 0 {
		grants = append(grants, supervisor.Capability{Name: "config", Handle: settings, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true})
	}
	event, result := launchRemoteExecutable(executable, "shell", grants, true)
	if result != supervisor.LaunchOK {
		syslog.Warn("remote shell launch failed", syslog.Uint("status", uint64(result)), syslog.Uint("session", worker.id))
		closeOpen(files)
		closeOpen(settings)
		closeOpen(handle.ID(tty[0]))
		failRemoteOpen(worker, remoteapi.ResultUnavailable)
		return
	}
	session.shellProcess, session.shellPID = event.ProcessHandle, event.Process
	session.execClient = 0
	session.active = true
	if remoteRegistry.CommitOpen(worker.id, worker.owner) != remoteapi.ResultOK || !replyRemote(worker, remoteapi.OperationOpen, remoteapi.ResultOK, []handle.ID{worker.tty, handle.ID(session.output[0])}, worker.requestCorrelation) {
		closeRemoteWorker(worker)
		return
	}
	worker.tty = 0
	session.output[0] = 0
	worker.opening = false
	worker.requestCorrelation = 0
}

func failRemoteOpen(worker *remoteWorker, result remoteapi.Result) {
	if worker.opening {
		replyRemote(worker, remoteapi.OperationOpen, result, nil, worker.requestCorrelation)
	} else {
		replyWorker(remoteapi.WorkerLaunch, 0, result, worker.requestCorrelation)
	}
	closeRemoteWorker(worker)
}

// releaseRemoteShell reaps every command before dropping the output writer.
// The worker retains the reader until all queued output has been sent.
func releaseRemoteShell(worker *remoteWorker) uint64 {
	session := &worker.session
	if session.shellProcess != 0 {
		remoteKill(session.shellProcess)
	}
	for index := range execJobs {
		job := &execJobs[index]
		if !job.active || job.session != session {
			continue
		}
		job.orphaned = true
		remoteKill(job.launcher.child)
		if job.preparing {
			dropFill(&job.fill)
			releasePrepared(job)
			closeLauncher(&job.launcher)
			resetRemoteJob(job)
			continue
		}
		for _, process := range job.job.Processes() {
			if process != 0 {
				remoteKill(process)
			}
		}
	}
	// Kill all stages before waiting for any one of them.
	for index := range execJobs {
		job := &execJobs[index]
		if !job.active || job.session != session {
			continue
		}
		for {
			process, pending := job.job.Next()
			if !pending {
				break
			}
			job.job.Complete(remoteWait(process))
		}
		closeLauncher(&job.launcher)
		resetRemoteJob(job)
	}

	code := uint64(0)
	if session.shellProcess != 0 {
		code = remoteWait(session.shellProcess)
	}
	resources := supervisor.RemoteResources{Mapping: session.pageBase, MappingSize: session.pageSize,
		Handles: [10]handle.ID{handle.ID(session.execServer), session.execClient, handle.ID(session.output[0]), handle.ID(session.output[1]), worker.tty, session.pageHandle}}
	if !resources.Release(nativeRemoteReleaser{}) {
		fatal("INIT_REMOTE_RELEASE_FAILED\n")
	}
	*session = terminalSession{}
	worker.tty = 0
	return code
}

func closeRemoteWorker(worker *remoteWorker) {
	if worker.id == 0 {
		return
	}
	remoteRegistry.BeginClose(worker.id, worker.owner)
	remoteKill(worker.process)
	releaseRemoteShell(worker)
	if worker.process != 0 {
		remoteWait(worker.process)
	}
	if worker.fill.session != 0 {
		dropFill(&worker.fill)
	}
	resources := supervisor.RemoteResources{Handles: [10]handle.ID{worker.control, worker.input, worker.output, worker.policy}}
	if !resources.Release(nativeRemoteReleaser{}) {
		fatal("INIT_REMOTE_RELEASE_FAILED\n")
	}
	remoteRegistry.FinishClose(worker.id, worker.owner)
	*worker = remoteWorker{}
	notifyRemoteCounts()
}

func injectRemoteSignerFault() bool {
	if !validation.Enabled {
		return false
	}
	for index := range remoteWorkers {
		worker := &remoteWorkers[index]
		if worker.id == 0 || worker.process == 0 || worker.control == 0 || worker.session.shellProcess == 0 {
			continue
		}
		if !sendMessage(uint64(worker.control), remoteapi.SignerFaultNotice()) {
			return false
		}
		mark("REMOTE_SIGNER_FAULT_INJECTED\n")
		return true
	}
	return false
}

func closeRemoteWorkers() {
	for index := range remoteWorkers {
		closeRemoteWorker(&remoteWorkers[index])
	}
}

func remoteExited(pid, code uint64) bool {
	if snapshot, found := manager.Snapshot(remoteServiceName); found && snapshot.PID == pid {
		closeRemoteWorkers()
		closeOpen(remoteControl)
		remoteControl = 0
		if remoteProcess != 0 {
			remoteWait(remoteProcess)
			remoteProcess = 0
		}
		return false
	}
	for index := range remoteWorkers {
		worker := &remoteWorkers[index]
		if worker.id == 0 {
			continue
		}
		if worker.pid == pid {
			closeRemoteWorker(worker)
			return true
		}
		if worker.session.shellPID == pid {
			beginRemoteShellClose(worker, code)
			return true
		}
	}
	return false
}

func remoteInterests(interests *[userwait.MaxObjects]userwait.Interest, count int, owners *[remoteapi.MaxWorkers*3 + 1]int, kinds *[remoteapi.MaxWorkers*3 + 1]uint8, tokens *[remoteapi.MaxWorkers*3 + 1]uint64) int {
	start := count
	add := func(id handle.ID, index int, kind uint8) {
		if id == 0 {
			return
		}
		interests[count] = userwait.Interest{Handle: id, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
		owners[count-start], kinds[count-start] = index, kind
		if index < 0 {
			tokens[count-start] = uint64(remoteControl)
		} else {
			tokens[count-start] = remoteWorkers[index].id
		}
		count++
	}
	add(remoteControl, -1, 0)
	for index := range remoteWorkers {
		worker := &remoteWorkers[index]
		if worker.id == 0 {
			continue
		}
		add(worker.control, index, 1)
		add(worker.fill.session, index, 2)
		add(handle.ID(worker.session.execServer), index, 3)
	}
	return count
}

func pollRemoteInterest(index int, kind uint8, token uint64) {
	if kind == 0 {
		if uint64(remoteControl) != token {
			return
		}
		pollRemoteListener()
		return
	}
	worker := &remoteWorkers[index]
	if worker.id == 0 || worker.id != token {
		return
	}
	switch kind {
	case 1:
		pollRemoteWorker(worker)
	case 2:
		if worker.fill.session != 0 {
			completeRemoteFill(worker)
		}
	case 3:
		if worker.session.execServer != 0 {
			pollExec(&worker.session)
		}
	}
}

func remoteTasks() int {
	tasks := 0
	for index := 0; index < supervisor.MaxServices; index++ {
		_, snapshot, found := manager.At(index)
		if !found {
			break
		}
		if snapshot.State == supervisor.StateRunning {
			tasks++
		}
	}
	for index := range remoteWorkers {
		worker := &remoteWorkers[index]
		if worker.id != 0 {
			tasks++
			if worker.opening || worker.session.active {
				tasks++
			}
		}
	}
	for index := range execJobs {
		job := &execJobs[index]
		if job.active {
			tasks += int(job.request.Count)
			if job.launcher.child != 0 || remoteSession(job.session) != nil {
				tasks++
			}
		}
	}
	return tasks
}

func remoteJobAllowed(session *terminalSession, request *execproto.Request) bool {
	if remoteSession(session) == nil {
		return true
	}
	jobs := 0
	for index := range execJobs {
		if execJobs[index].active && remoteSession(execJobs[index].session) != nil {
			jobs++
		}
	}
	return supervisor.RemoteJobAllowed(jobs, remoteTasks(), int(request.Count), true)
}

func remoteFillDeadline(earliest uint64, pending bool) (uint64, bool) {
	if remoteCountsPending && (!pending || remoteCountsDeadline < earliest) {
		earliest, pending = remoteCountsDeadline, true
	}
	for index := range remoteWorkers {
		grace := &remoteWorkers[index].grace
		if grace.Session != 0 {
			deadline := grace.Deadline
			if grace.NoticePending && grace.Retry < deadline {
				deadline = grace.Retry
			}
			if !pending || deadline < earliest {
				earliest, pending = deadline, true
			}
		}
		fill := &remoteWorkers[index].fill
		if fill.session != 0 && (!pending || fill.deadline < earliest) {
			earliest, pending = fill.deadline, true
		}
	}
	return earliest, pending
}
func expireRemoteFills(now uint64) {
	if remoteCountsPending && now >= remoteCountsDeadline {
		flushRemoteCounts()
	}
	for index := range remoteWorkers {
		worker := &remoteWorkers[index]
		if worker.grace.Expired(worker.id, now) {
			closeRemoteWorker(worker)
			continue
		}
		if worker.id != 0 && worker.grace.NoticePending && now >= worker.grace.Retry {
			sendRemoteTerminalEnd(worker)
		}
		if worker.id != 0 && worker.fill.session != 0 && now >= worker.fill.deadline {
			failRemoteOpen(worker, remoteapi.ResultUnavailable)
		}
	}
}

func stopRemoteService() {
	closeRemoteWorkers()
	if remoteProcess != 0 {
		remoteKill(remoteProcess)
		remoteWait(remoteProcess)
		remoteProcess = 0
	}
	closeOpen(remoteControl)
	remoteControl = 0
}
func remoteDependencyExited(pid uint64) {
	for _, name := range [...]string{"vfs", "config", netServiceName, filesystemName, "block", driverName} {
		snapshot, found := manager.Snapshot(name)
		if found && snapshot.State == supervisor.StateRunning && snapshot.PID == pid {
			stopRemoteService()
			return
		}
	}
}

type nativeRemoteReleaser struct{}

func (nativeRemoteReleaser) Close(id handle.ID) bool { return sys.HandleClose(id) == 0 }
func (nativeRemoteReleaser) Unmap(base, size uint64) bool {
	return sys.VMUnmap(base, size) == kcall.StatusOK
}

func resetRemoteJob(job *execJob) {
	job.active, job.orphaned, job.preparing, job.cancelled = false, false, false, false
	job.session = nil
	job.request = execproto.Request{}
	job.job = supervisor.Job{}
	clear(job.data[:])
}

// ProcessWait does not consume the kernel's lifecycle notification. Drain it
// between kills so a bulk disconnect cannot overflow the endpoint/retry queues.
func remoteKill(process handle.ID) {
	if process != 0 {
		sys.ProcessKill(process)
		drainRemoteLifecycle()
	}
}
func remoteWait(process handle.ID) uint64 {
	code := waitApplicationExit(process)
	drainRemoteLifecycle()
	return code
}

func openRemoteAdmin() handle.ID {
	if remoteControl == 0 {
		return 0
	}
	var pair [2]uint64
	if !createChannel(&pair) {
		return 0
	}
	notice, ok := remoteapi.AdminSessionNotice(handle.ID(pair[1]))
	if !ok || !sendMessage(uint64(remoteControl), notice) {
		closePair(pair)
		return 0
	}
	return handle.ID(pair[0])
}
func notifyRemoteCounts() { remoteCountsPending = true; flushRemoteCounts() }
func flushRemoteCounts() {
	if remoteControl == 0 {
		remoteCountsPending, remoteCountsDeadline = false, 0
		return
	}
	var pending, authenticated uint8
	for index := range remoteWorkers {
		worker := &remoteWorkers[index]
		if worker.id == 0 {
			continue
		}
		record, result := remoteRegistry.Snapshot(worker.id, worker.owner)
		if result != remoteapi.ResultOK {
			continue
		}
		if record.Authenticated {
			authenticated++
		} else {
			pending++
		}
	}
	if notice, ok := remoteapi.AdminCountsNotice(pending, authenticated); ok && sendMessage(uint64(remoteControl), notice) {
		remoteCountsPending, remoteCountsDeadline = false, 0
		return
	}
	now, _ := sys.ClockRead()
	remoteCountsDeadline = now + 1
}

func beginRemoteShellClose(worker *remoteWorker, code uint64) {
	if worker.id == 0 || worker.grace.Session != 0 {
		return
	}
	if worker.process == 0 || worker.control == 0 {
		closeRemoteWorker(worker)
		return
	}
	now, _ := sys.ClockRead()
	if !worker.grace.Begin(worker.id, code, now, 5*clock.TicksPerSecond) {
		closeRemoteWorker(worker)
		return
	}
	remoteRegistry.BeginClose(worker.id, worker.owner)
	worker.grace.Code = releaseRemoteShell(worker)
	sendRemoteTerminalEnd(worker)
	notifyRemoteCounts()
}

func sendRemoteTerminalEnd(worker *remoteWorker) {
	if !worker.grace.NoticePending {
		return
	}
	notice, ok := remoteapi.TerminalEndNotice(worker.id, worker.grace.Code)
	if ok && sendMessage(uint64(worker.control), notice) {
		worker.grace.Delivered(worker.id)
		return
	}
	now, _ := sys.ClockRead()
	worker.grace.Retry = now + 1
}
