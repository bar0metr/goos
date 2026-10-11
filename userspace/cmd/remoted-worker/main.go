//go:build goos_userspace

// Command remoted-worker owns one SSH connection and one terminal engine.
package main

import (
	"crypto/ed25519"
	"goos/internal/validation"
	"goos/kernel/clock"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	syslog "goos/userspace/log"
	"goos/userspace/remoteapi"
	"goos/userspace/remoteservice"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/sys"
	"goos/userspace/sshserver"
	"goos/userspace/startup"
	"goos/userspace/tty"
)

var control, terminal, stdout, stderr, stdin, signerChannel handle.ID
var signingClient *hostSigner
var token, sequence uint64
var server *sshserver.Server
var engine *tty.Engine
var output, errorOutput remoteservice.Queue
var pendingInput inputQueue
var execMode, inputEOF, nextError bool
var newlines ptyNewlines
var policy remoteapi.WorkerPolicy
var conn remoteservice.RelayConn
var input [sshserver.MaxCiphertext]byte
var scratch [256]byte
var geometry remoteapi.Geometry
var terminalGone, ending bool
var handshaken, peerClosed bool
var endDeadline uint64
var exitCode uint32
var exitKnown bool

//export userspaceMain
func userspaceMain() {
	arena.Init()
	arena.OnExhausted(func() { sys.Exit(1) })
	arena.InitPackages()
	data, ok := startup.Read()
	if !ok {
		sys.Exit(1)
	}
	logHandle, ok := data.Handle("log")
	if !ok {
		sys.Exit(1)
	}
	syslog.Init(logHandle, "remoted-worker")
	control, ok = data.Handle("control")
	if !ok {
		sys.Exit(1)
	}
	signerChannel, ok = data.Handle("signer")
	if !ok {
		sys.Exit(1)
	}
	conn.Input, ok = data.Handle("input")
	if !ok {
		sys.Exit(1)
	}
	conn.Output, ok = data.Handle("output")
	if !ok {
		sys.Exit(1)
	}
	memory, ok := data.Handle("policy")
	if !ok {
		sys.Exit(1)
	}
	base, size, status := sys.MemoryMapAnywhere(memory, false)
	if status != kcall.StatusOK || size != remoteapi.PolicyPageSize || !remoteapi.DecodePolicy(sys.Window(base, size), &policy) {
		sys.Exit(1)
	}
	conn.TimeoutTicks = uint64(policy.HandshakeTimeoutMS)*clock.TicksPerSecond/1000 + 1
	keys := make([]ed25519.PublicKey, int(policy.KeyCount))
	for i := range keys {
		keys[i] = policy.Keys[i][:]
	}
	signingClient, ok = newHostSigner(ed25519.PublicKey(policy.HostPublicKey[:]), policy.Generation, signerTransport{
		send: func(message *ipc.Message) bool { return remoteservice.Send(signerChannel, message) },
		receive: func(ticks uint64, message *ipc.Message) kcall.Status {
			return remoteservice.Receive(signerChannel, ticks, message)
		},
		closeHandles: remoteservice.CloseHandles,
		close:        closeHostSigner,
		now:          remoteservice.Now,
	})
	if !ok {
		sys.Exit(1)
	}
	username := string(policy.Username[:policy.UserLen])
	var err error
	server, err = sshserver.New(&conn, sshserver.Config{HostPublicKey: ed25519.PublicKey(policy.HostPublicKey[:]), SignHost: signingClient.Sign, Username: username, AuthorizedKeys: keys, MaxAuthAttempts: int(policy.MaxAuthAttempts)})
	if err != nil {
		finish(1)
	}
	signingClient.deadline = remoteservice.Now() + conn.TimeoutTicks
	conn.HandshakeDeadline(conn.TimeoutTicks)
	if server.Handshake() != nil || server.User() != username {
		syslog.Warn("remote SSH authentication or handshake failed")
		finish(1)
	}
	conn.ClearDeadline()
	signingClient.deadline = 0
	handshaken = true
	authenticated, _, ok := call(remoteapi.Request{Operation: remoteapi.OperationAuthenticate})
	if !ok {
		syslog.Warn("remote session admission refused")
		finish(1)
	}
	token = authenticated.Session
	syslog.Info("remote authentication accepted", syslog.Str("user", username), syslog.Uint("session", token))
	engine = tty.NewEngine(func(bytes []byte) bool {
		return newlines.Write(&output, bytes, engine != nil && engine.Raw())
	}, func(message ipc.Message) bool { return remoteservice.Send(terminal, &message) })
	geometry, _ = remoteapi.MakeGeometry(80, 24, []byte("xterm"))
	lastActivity := remoteservice.Now()
	transient := arena.Current()
	for {
		arena.Reset(transient)
		activity := false
		var notice ipc.Message
		noticeStatus := remoteservice.Receive(control, 0, &notice)
		if noticeStatus == kcall.StatusOK {
			if validation.Enabled && remoteapi.DecodeSignerFaultNotice(&notice) {
				closeHostSigner()
			} else if !terminalEnd(&notice) {
				remoteservice.CloseHandles(&notice)
				finish(1)
			}
			activity = true
		} else if noticeStatus != kcall.StatusNotFound {
			finish(1)
		}
		if !drainOutput() {
			finish(1)
		}
		activity = serviceStdin() || activity
		for budget := 0; budget < 8; budget++ {
			event, err := server.Poll()
			if err != nil {
				syslog.Warn("remote SSH protocol violation")
				finish(1)
			}
			if event.Kind == sshserver.NeedInput {
				break
			}
			activity = true
			switch event.Kind {
			case sshserver.PTY, sshserver.Resize:
				next, valid := geometryForEvent(geometry, event)
				if !valid {
					finish(1)
				}
				if terminal != 0 {
					if !installGeometry(engine, &next) {
						finish(1)
					}
					if _, _, ok := call(remoteapi.Request{Operation: remoteapi.OperationGeometry, Session: token, Geometry: next}); !ok {
						finish(1)
					}
				}
				geometry = next
			case sshserver.Shell:
				if terminal != 0 || execMode {
					finish(1)
				}
				if !installGeometry(engine, &geometry) {
					finish(1)
				}
				opened, attachments, ok := call(remoteapi.Request{Operation: remoteapi.OperationOpen, Geometry: geometry})
				if !ok || opened.Session != token {
					syslog.Warn("remote shell open failed", syslog.Uint("result", uint64(opened.Result)), syslog.Uint("session", token))
					finish(1)
				}
				terminal, stdout = attachments[0], attachments[1]
				syslog.Info("remote terminal opened", syslog.Uint("session", token), syslog.Uint("columns", uint64(geometry.Columns)), syslog.Uint("rows", uint64(geometry.Rows)))
			case sshserver.Exec:
				if terminal != 0 || execMode || len(event.Command) == 0 || len(event.Command) > remoteapi.MaxCommand {
					finish(1)
				}
				execMode = true
				if !openExec(event.Command) {
					finish(1)
				}
			case sshserver.Data:
				if ending || terminalGone {
					break
				}
				if execMode {
					if stdin == 0 {
						if server.ConsumeInput(uint32(len(event.Data))) != nil {
							finish(1)
						}
					} else if !pendingInput.put(event.Data) {
						finish(1)
					}
					break
				}
				if terminal == 0 {
					finish(1)
				}
				for _, value := range event.Data {
					engine.Feed(value)
				}
				engine.FlushInput()
			case sshserver.EOF:
				inputEOF = true
				if execMode {
					break
				}
				if terminal != 0 && !terminalGone {
					engine.Feed(4)
					engine.FlushInput()
				}
			case sshserver.Close:
				peerClosed = true
				finish(0)
			}
			if engine.Failed() || !drainOutput() {
				finish(1)
			}
		}
		if terminal != 0 && !terminalGone {
			engine.FlushGeometry()
		}
		need := server.NeedInput()
		if need > 0 {
			n, status := sys.StreamRead(conn.Input, input[:min(need, len(input), int(kcall.MaxStreamIO))])
			if status == kcall.StatusOK && n > 0 {
				used, err := server.Feed(input[:n])
				if err != nil || used != n {
					finish(1)
				}
				activity = true
			} else if status != kcall.StatusResourceExhausted && status != kcall.StatusOK {
				finish(0)
			}
		}
		// The shell can send its next prompt before its preceding stdout bytes
		// reach this worker. Keep a bounded batch ahead of terminal requests.
		stdoutPending, stdoutActivity := serviceStdout()
		activity = activity || stdoutActivity
		if execMode {
			activity = serviceStderr() || activity
			activity = serviceStdin() || activity
		}
		if terminal != 0 && !terminalGone && !stdoutPending {
			var message ipc.Message
			status := remoteservice.Receive(terminal, 0, &message)
			if status == kcall.StatusOK {
				if message.HandleCount != 0 {
					remoteservice.CloseHandles(&message)
					finish(1)
				}
				engine.Request(&message)
				activity = true
			} else if status == kcall.StatusPeerDied {
				terminalGone = true
				if endDeadline == 0 {
					endDeadline = remoteservice.Now() + 5*clock.TicksPerSecond
				}
			} else if status != kcall.StatusNotFound {
				finish(1)
			}
		}
		if engine.Failed() {
			finish(1)
		}
		now := remoteservice.Now()
		if ending && outputReadyToExit(exitKnown, stdout, stderr, output.Len(), errorOutput.Len(), server.KeyExchangePending()) {
			finish(exitCode)
		}
		if endDeadline != 0 && now >= endDeadline {
			if exitKnown {
				finish(exitCode)
			} else {
				finish(1)
			}
		}
		if activity {
			lastActivity = now
		} else {
			if policy.IdleTimeoutSeconds != 0 && now-lastActivity >= uint64(policy.IdleTimeoutSeconds)*clock.TicksPerSecond {
				syslog.Info("remote idle timeout", syslog.Uint("session", token))
				finish(0)
			}
			wait := uint64(clock.TicksPerSecond)
			if policy.IdleTimeoutSeconds != 0 {
				deadline := lastActivity + uint64(policy.IdleTimeoutSeconds)*clock.TicksPerSecond
				wait = min(wait, deadline-now)
			}
			var interests [kcall.WaitAnyMaxObjects]sys.Interest
			interests[0] = sys.Interest{Handle: control, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
			count := 1
			if need > 0 {
				interests[count] = sys.Interest{Handle: conn.Input, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
				count++
			}
			if terminal != 0 && !terminalGone {
				interests[count] = sys.Interest{Handle: terminal, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
				if engine.GeometryPending() {
					interests[count].Events |= kcall.WaitWritable
				}
				count++
			}
			if stdout != 0 && output.Free() >= 1024 {
				interests[count] = sys.Interest{Handle: stdout, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
				count++
			}
			if stderr != 0 && errorOutput.Free() >= 1024 {
				interests[count] = sys.Interest{Handle: stderr, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
				count++
			}
			if stdin != 0 && pendingInput.size > 0 {
				interests[count] = sys.Interest{Handle: stdin, Events: kcall.WaitWritable | kcall.WaitPeerClosed}
				count++
			}
			if count == 0 {
				finish(1)
			}
			_, status := sys.WaitAny(&interests, count, max(wait, 1))
			if status != kcall.StatusOK && status != kcall.StatusTimeout {
				finish(1)
			}
		}
	}
}

const stdoutBatch = 8

// serviceStdout returns true while stdout may still precede a prompt. The
// caller continues SSH input and control processing between batches.
func serviceStdout() (pending, activity bool) {
	if stdout == 0 {
		return false, false
	}
	pending, activity, closed, ok := drainStdoutBatch(readWorkerStdout, consumeWorkerStdout, workerOutputFree, scratch[:], stdoutBatch)
	if !ok {
		finish(1)
	}
	if closed {
		sys.HandleClose(stdout)
		stdout = 0
		ending = !execMode || stderr == 0
		if endDeadline == 0 {
			endDeadline = remoteservice.Now() + 5*clock.TicksPerSecond
		}
	}
	return pending, activity
}

func readWorkerStdout(buffer []byte) (int, kcall.Status) { return sys.StreamRead(stdout, buffer) }
func consumeWorkerStdout(buffer []byte) {
	if execMode {
		if !output.Put(buffer) {
			finish(1)
		}
	} else {
		engine.Output(buffer)
	}
}
func workerOutputFree() int { return output.Free() }
func call(request remoteapi.Request) (remoteapi.Reply, [3]handle.ID, bool) {
	var message ipc.Message
	var reply remoteapi.Reply
	var attachments [3]handle.ID
	sequence++
	if !remoteapi.EncodeRequest(&request, sequence, &message) || !remoteservice.Send(control, &message) {
		return reply, attachments, false
	}
	deadline := remoteservice.Now() + 5*clock.TicksPerSecond
	for remoteservice.Now() < deadline {
		if remoteservice.Receive(control, deadline-remoteservice.Now(), &message) != kcall.StatusOK {
			return reply, attachments, false
		}
		if terminalEnd(&message) {
			continue
		}
		if !remoteapi.DecodeReply(&message, sequence, request.Operation, &reply) {
			remoteservice.CloseHandles(&message)
			return reply, attachments, false
		}
		copy(attachments[:], message.Handles[:message.HandleCount])
		return reply, attachments, reply.Result == remoteapi.ResultOK
	}
	return reply, attachments, false
}
func terminalEnd(message *ipc.Message) bool {
	session, code, ok := remoteapi.DecodeTerminalEndNotice(message)
	if !ok || session != token || token == 0 {
		return false
	}
	exitKnown = true
	exitCode = sshExitCode(code)
	if endDeadline == 0 {
		endDeadline = remoteservice.Now() + 5*clock.TicksPerSecond
	}
	return true
}
func drainOutput() bool {
	return drainOutputPair(&output, &errorOutput, &nextError, func(data []byte, extended bool) (int, error) {
		if extended {
			return server.WriteStderr(data)
		}
		return server.Write(data)
	}, 8)
}
func finish(status uint32) {
	syslog.Info("remote session closing", syslog.Uint("session", token), syslog.Uint("status", uint64(status)))
	if server != nil {
		if handshaken {
			deadline := remoteservice.Now() + 5*clock.TicksPerSecond
			if endDeadline != 0 {
				deadline = min(deadline, endDeadline)
			}
			now := remoteservice.Now()
			if now < deadline {
				conn.HandshakeDeadline(deadline - now)
				if server.Exit(status) == nil && !peerClosed {
					awaitPeerClose(deadline)
				}
			}
		}
		server.Close()
	}
	if engine != nil {
		engine.CloseSession()
	}
	closeHostSigner()
	conn.Close()
	// PID 1 may terminate this worker as soon as Close is delivered.
	if token != 0 {
		call(remoteapi.Request{Operation: remoteapi.OperationClose, Session: token})
		token = 0
	}
	sys.Exit(uint64(status))
}
func awaitPeerClose(deadline uint64) {
	for remoteservice.Now() < deadline {
		event, err := server.Poll()
		if err != nil || event.Kind == sshserver.Close {
			return
		}
		if event.Kind != sshserver.NeedInput {
			continue
		}
		if terminal != 0 && !terminalGone {
			engine.FlushGeometry()
		}
		need := server.NeedInput()
		if need < 1 || need > len(input) {
			return
		}
		n, status := sys.StreamRead(conn.Input, input[:min(need, int(kcall.MaxStreamIO))])
		if status == kcall.StatusOK && n > 0 {
			used, err := server.Feed(input[:n])
			if err != nil || used != n {
				return
			}
			continue
		}
		if status != kcall.StatusResourceExhausted {
			return
		}
		now := remoteservice.Now()
		if now >= deadline {
			return
		}
		var interests [kcall.WaitAnyMaxObjects]sys.Interest
		interests[0] = sys.Interest{Handle: conn.Input, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
		_, status = sys.WaitAny(&interests, 1, deadline-now)
		if status != kcall.StatusOK {
			return
		}
	}
}
func main() {}

func closeHostSigner() {
	if signerChannel != 0 {
		sys.HandleClose(signerChannel)
		signerChannel = 0
	}
}

// The request transfers a copy of this memory handle. PID 1 takes a bounded
// snapshot before launching the fixed shell image and supplies only streams.
func openExec(command []byte) bool {
	memory, status := sys.MemoryCreate(1)
	if status != kcall.StatusOK {
		return false
	}
	defer sys.HandleClose(memory)
	base, size, status := sys.MemoryMapAnywhere(memory, true)
	if status != kcall.StatusOK {
		return false
	}
	if size != 4096 {
		sys.VMUnmap(base, size)
		return false
	}
	copy(sys.Window(base, size), command)
	if sys.VMUnmap(base, size) != kcall.StatusOK {
		return false
	}
	opened, attachments, ok := call(remoteapi.Request{Operation: remoteapi.OperationOpen, Mode: remoteapi.ModeExec, Command: memory, CommandSize: uint16(len(command))})
	if !ok || opened.Session != token || opened.Mode != remoteapi.ModeExec {
		return false
	}
	stdin, stdout, stderr = attachments[0], attachments[1], attachments[2]
	return true
}

func serviceStdin() bool {
	if !execMode || stdin == 0 {
		return false
	}
	activity := false
	for budget := 0; budget < 8 && pendingInput.size > 0; budget++ {
		data := pendingInput.peek()
		count, status := sys.StreamWrite(stdin, data)
		if count < 0 || count > len(data) {
			finish(1)
		}
		if status == kcall.StatusPeerDied {
			sys.HandleClose(stdin)
			stdin = 0
			if server.ConsumeInput(uint32(pendingInput.size)) != nil {
				finish(1)
			}
			pendingInput.drop(pendingInput.size)
			return true
		}
		if status != kcall.StatusOK && status != kcall.StatusResourceExhausted {
			finish(1)
		}
		if count == 0 {
			break
		}
		pendingInput.drop(count)
		if server.ConsumeInput(uint32(count)) != nil {
			finish(1)
		}
		activity = true
	}
	if inputEOF && pendingInput.size == 0 {
		sys.HandleClose(stdin)
		stdin = 0
		activity = true
	}
	return activity
}

func serviceStderr() bool {
	if stderr == 0 {
		return false
	}
	_, activity, closed, ok := drainStdoutBatch(func(buffer []byte) (int, kcall.Status) { return sys.StreamRead(stderr, buffer) }, func(buffer []byte) {
		if !errorOutput.Put(buffer) {
			finish(1)
		}
	}, func() int { return errorOutput.Free() }, scratch[:], stdoutBatch)
	if !ok {
		finish(1)
	}
	if closed {
		sys.HandleClose(stderr)
		stderr = 0
		ending = stdout == 0
		if endDeadline == 0 {
			endDeadline = remoteservice.Now() + 5*clock.TicksPerSecond
		}
	}
	return activity
}
