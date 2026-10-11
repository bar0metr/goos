package main

import (
	"goos/kernel/ipc"
	supervisor "goos/userspace/init"
	"goos/userspace/remoteapi"
	"goos/userspace/sshserver"
	"goos/userspace/tty"
	"testing"
)

func TestWindowChangePreservesPTYTerm(t *testing.T) {
	current, ok := remoteapi.MakeGeometry(80, 24, []byte("xterm-256color"))
	if !ok {
		t.Fatal("fixture")
	}
	next, ok := geometryForEvent(current, sshserver.Event{Kind: sshserver.Resize, Columns: 100, Rows: 30})
	if !ok || next.Columns != 100 || next.Rows != 30 || string(next.Term[:next.TermSize]) != "xterm-256color" {
		t.Fatal("resize lost geometry or TERM", next)
	}
	if current.Columns != 80 || current.Rows != 24 {
		t.Fatal("mutated prior geometry")
	}
}

func TestPTYGeometryUsesRequestTerm(t *testing.T) {
	event := sshserver.Event{Kind: sshserver.PTY, Columns: 80, Rows: 24, TermLen: 5}
	copy(event.Term[:], "vt100")
	next, ok := geometryForEvent(remoteapi.Geometry{}, event)
	if !ok || string(next.Term[:next.TermSize]) != "vt100" {
		t.Fatal("PTY TERM ignored")
	}
	for _, size := range []uint8{0, 65, 255} {
		event.TermLen = size
		if _, ok := geometryForEvent(remoteapi.Geometry{}, event); ok {
			t.Fatal("invalid TERM size", size)
		}
	}
}

func TestWindowChangeRejectsInvalidGeometry(t *testing.T) {
	current, _ := remoteapi.MakeGeometry(80, 24, []byte("xterm"))
	for _, event := range []sshserver.Event{
		{Kind: sshserver.Resize, Columns: 0, Rows: 24},
		{Kind: sshserver.Resize, Columns: 80, Rows: 1001},
		{Kind: sshserver.Data, Columns: 80, Rows: 24},
	} {
		if _, ok := geometryForEvent(current, event); ok {
			t.Fatal("invalid event accepted", event.Kind)
		}
	}
	current.TermSize = 255
	if _, ok := geometryForEvent(current, sshserver.Event{Kind: sshserver.Resize, Columns: 80, Rows: 24}); ok {
		t.Fatal("invalid current TERM accepted")
	}
}

func TestPreOpenAndCoalescedResizeForwardOneSequence(t *testing.T) {
	geometry, _ := remoteapi.MakeGeometry(80, 24, []byte("xterm"))
	for _, columns := range []uint32{90, 100, 120, 120} {
		geometry, _ = geometryForEvent(geometry, sshserver.Event{Kind: sshserver.Resize, Columns: columns, Rows: 40})
	}
	var notices []ipc.Message
	engine := tty.NewEngine(func([]byte) bool { return true }, func(message ipc.Message) bool {
		if message.Kind == ipc.KindNotification {
			notices = append(notices, message)
		}
		return true
	})
	if !installGeometry(engine, &geometry) || geometry.Sequence != 1 {
		t.Fatal("pre-open burst changed startup sequence")
	}
	startupGeometry := engine.CurrentGeometry()
	var openMessage ipc.Message
	if !remoteapi.EncodeRequest(&remoteapi.Request{Operation: remoteapi.OperationOpen, Geometry: geometry}, 1, &openMessage) {
		t.Fatal("Open encode")
	}
	var openRequest remoteapi.Request
	if !remoteapi.DecodeRequest(&openMessage, &openRequest) || openRequest.Geometry.Sequence != startupGeometry.Sequence {
		t.Fatal("Open and GTTY startup sequence diverged")
	}
	var registry supervisor.RemoteRegistry
	id, _ := registry.Admit(7, 1)
	registry.Authenticate(id, 7, 1)
	if registry.BeginOpen(id, 7, openRequest.Geometry) != remoteapi.ResultOK {
		t.Fatal("open")
	}
	registry.CommitOpen(id, 7)
	request, _ := tty.Request(tty.OperationGeometry, nil, 1)
	engine.Request(&request)
	for _, columns := range []uint32{130, 140, 140, 150} {
		next, _ := geometryForEvent(geometry, sshserver.Event{Kind: sshserver.Resize, Columns: columns, Rows: 45})
		if !installGeometry(engine, &next) {
			t.Fatal("install")
		}
		var message ipc.Message
		remoteapi.EncodeRequest(&remoteapi.Request{Operation: remoteapi.OperationGeometry, Session: id, Geometry: next}, next.Sequence, &message)
		var forwarded remoteapi.Request
		if !remoteapi.DecodeRequest(&message, &forwarded) || forwarded.Geometry.Sequence != engine.CurrentGeometry().Sequence {
			t.Fatal("wire dropped geometry sequence")
		}
		registry.Resize(id, 7, forwarded.Geometry)
		registry.Resize(id, 7, forwarded.Geometry)
		engine.FlushGeometry()
		geometry = next
	}
	if len(notices) != 1 {
		t.Fatal("geometry queue not coalesced")
	}
	first, _ := tty.DecodeGeometryNotice(&notices[0])
	ack := tty.GeometryAcknowledgement(first.Sequence)
	engine.Request(&ack)
	engine.FlushGeometry()
	latest, _ := tty.DecodeGeometryNotice(&notices[len(notices)-1])
	record, _ := registry.Snapshot(id, 7)
	if latest.Sequence != 5 || record.Geometry.Sequence != latest.Sequence || geometry.Sequence != latest.Sequence || latest.Columns != 150 {
		t.Fatal("GTTY/PID1 sequence diverged")
	}
	var child tty.Snapshot
	child.Apply(latest)
	if child.Apply(startupGeometry) {
		t.Fatal("late initial snapshot overwrote coalesced resize")
	}
}
