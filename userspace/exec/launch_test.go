package exec

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"testing"
)

func TestLaunchMessagesRoundTrip(t *testing.T) {
	message, ok := EncodeLaunch(64, handle.ID(7), 3)
	if !ok {
		t.Fatal("encode submit")
	}
	if size, ok := DecodeLaunch(message); !ok || size != 64 || message.Handles[0] != 7 {
		t.Fatalf("decoded %d %v", size, ok)
	}
	reply, ok := EncodeLaunchReply(LaunchReply{ExitCode: 9}, 3)
	if !ok {
		t.Fatal("encode reply")
	}
	if decoded, ok := DecodeLaunchReply(reply); !ok || decoded.ExitCode != 9 || decoded.LaunchStatus != 0 {
		t.Fatalf("decoded reply %+v %v", decoded, ok)
	}
}

func TestLaunchMessagesRefuseMalformed(t *testing.T) {
	for _, bad := range []struct {
		size   int
		object handle.ID
		corr   uint64
	}{{64, 7, 0}, {64, 0, 1}, {headerSize - 1, 7, 1}, {MaxLaunchObject + 1, 7, 1}} {
		if _, ok := EncodeLaunch(bad.size, bad.object, bad.corr); ok {
			t.Fatalf("encoded %+v", bad)
		}
	}
	valid, _ := EncodeLaunch(64, 7, 3)
	damage := []func(*ipc.Message){
		func(m *ipc.Message) { m.ProtocolID = ProtocolID },
		func(m *ipc.Message) { m.Version++ },
		func(m *ipc.Message) { m.Kind = ipc.KindReply },
		func(m *ipc.Message) { m.Correlation = 0 },
		func(m *ipc.Message) { m.HandleCount = 0 },
		func(m *ipc.Message) { m.HandleCount, m.Handles[1] = 2, 8 },
		func(m *ipc.Message) { m.Handles[0] = 0 },
		func(m *ipc.Message) { m.PayloadSize++ },
		func(m *ipc.Message) { m.Payload[2] = 0xff }, // a request larger than a slot
	}
	for index, apply := range damage {
		message := valid
		apply(&message)
		if _, ok := DecodeLaunch(message); ok {
			t.Fatalf("damage %d accepted", index)
		}
	}
	if _, ok := EncodeLaunchReply(LaunchReply{LaunchStatus: 2, ExitCode: 1}, 1); ok {
		t.Fatal("a failed launch with an exit code encoded")
	}
	reply, _ := EncodeLaunchReply(LaunchReply{LaunchStatus: 2}, 1)
	reply.HandleCount = 1
	if _, ok := DecodeLaunchReply(reply); ok {
		t.Fatal("a reply carrying a handle decoded")
	}
}

func launchRequest(t *testing.T, build func(*Builder)) Request {
	t.Helper()
	var slot [SlotSize]byte
	builder := NewBuilder(slot[:], []byte("/home"), [][]byte{[]byte("COLUMNS=80")}, false)
	build(&builder)
	data, ok := builder.Finish(0)
	if !ok {
		t.Fatal("finish")
	}
	request, ok := DecodeRequest(data)
	if !ok {
		t.Fatal("decode")
	}
	return request
}

func TestLaunchCommandAcceptsOneSystemCommand(t *testing.T) {
	request := launchRequest(t, func(builder *Builder) {
		builder.Stage([]byte("/system/bin/gview"), [][]byte{[]byte("gview"), []byte("/home/a.txt")})
	})
	if command, ok := request.LaunchCommand(); !ok || string(command) != "gview" {
		t.Fatalf("command %q %v", command, ok)
	}
}

func TestLaunchCommandRefusesEverythingElse(t *testing.T) {
	outside := launchRequest(t, func(builder *Builder) {
		builder.Stage([]byte("/home/tool"), [][]byte{[]byte("tool")})
	})
	service := launchRequest(t, func(builder *Builder) {
		builder.Stage([]byte("/system/vfs"), [][]byte{[]byte("vfs")})
	})
	pipeline := launchRequest(t, func(builder *Builder) {
		builder.Stage([]byte("/system/bin/gview"), [][]byte{[]byte("gview")})
		builder.Stage([]byte("/system/bin/cat"), [][]byte{[]byte("cat")})
	})
	background := pipeline
	background.Count, background.Background = 1, true
	for name, request := range map[string]Request{"outside": outside, "service": service, "pipeline": pipeline, "background": background} {
		if _, ok := request.LaunchCommand(); ok {
			t.Fatalf("%s accepted", name)
		}
	}
}
