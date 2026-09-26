package exec

import (
	"bytes"
	"encoding/binary"
	"goos/internal/limits"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/mm/vm"
	"goos/userspace/startup"
	"testing"
)

func words(values ...string) [][]byte {
	result := make([][]byte, len(values))
	for index, value := range values {
		result[index] = []byte(value)
	}
	return result
}

func strings(list List) []string {
	var result []string
	for offset, index := 0, 0; index < list.Count(); index++ {
		var value []byte
		value, offset = list.Next(offset)
		result = append(result, string(value))
	}
	return result
}

func TestProtocolRoundTrip(t *testing.T) {
	var slot [SlotSize]byte
	data, ok := EncodeRequest(Spec{Stages: [MaxStages]StageSpec{{Path: []byte("/system/bin/cat"), Arguments: words("cat", "motd")}}, Count: 1, Background: true, CWD: []byte("/system"), Environment: words("PATH=/system/bin")}, slot[:])
	if !ok {
		t.Fatal("request encode failed")
	}
	request, ok := DecodeRequest(data)
	if !ok || string(request.Stages[0].Path) != "/system/bin/cat" || string(request.CWD) != "/system" || !request.Background {
		t.Fatalf("request = %#v", request)
	}
	if got := strings(request.Stages[0].Arguments); len(got) != 2 || got[0] != "cat" || got[1] != "motd" {
		t.Fatalf("arguments = %q", got)
	}
	if got := strings(request.Environment); len(got) != 1 || got[0] != "PATH=/system/bin" {
		t.Fatalf("environment = %q", got)
	}
	replyMessage, ok := EncodeReply(Reply{Process: 12, ExitCode: 7}, 7)
	if !ok {
		t.Fatal("reply encode failed")
	}
	reply, ok := DecodeReply(replyMessage)
	if !ok || reply.Process != 12 || reply.LaunchStatus != 0 || reply.ExitCode != 7 {
		t.Fatalf("reply = %#v", reply)
	}
}

func pipelineSpec() Spec {
	spec := Spec{Count: MaxStages, CWD: []byte("/")}
	for index := range spec.Stages {
		spec.Stages[index] = StageSpec{Path: []byte("/system/bin/readline"), Arguments: words("readline")}
	}
	return spec
}

func TestPipelineProtocolRoundTrip(t *testing.T) {
	for count := uint8(1); count <= MaxStages; count++ {
		original := pipelineSpec()
		original.Count = count
		var slot [SlotSize]byte
		data, ok := EncodeRequest(original, slot[:])
		if !ok {
			t.Fatal("pipeline encode failed")
		}
		request, ok := DecodeRequest(data)
		if !ok || request.Count != count || !bytes.Equal(request.CWD, original.CWD) || request.Environment.Count() != 0 {
			t.Fatalf("pipeline request = %#v", request)
		}
		for i := 0; i < int(count); i++ {
			if !bytes.Equal(request.Stages[i].Path, original.Stages[i].Path) || strings(request.Stages[i].Arguments)[0] != "readline" {
				t.Fatal("stage changed")
			}
		}
	}
}

func TestPipelineProtocolRoundTripsErrorSource(t *testing.T) {
	for count := uint8(2); count <= MaxStages; count++ {
		for source := uint8(1); source < count; source++ {
			original := pipelineSpec()
			original.Count, original.ErrorSource = count, source
			var slot [SlotSize]byte
			data, ok := EncodeRequest(original, slot[:])
			if !ok {
				t.Fatalf("encode count=%d source=%d", count, source)
			}
			request, ok := DecodeRequest(data)
			if !ok || request.ErrorSource != source || request.Count != count {
				t.Fatalf("decoded request = %#v", request)
			}
		}
	}
}

func TestPipelineProtocolRejectsMalformedData(t *testing.T) {
	var slot [SlotSize]byte
	spec := pipelineSpec()
	spec.Environment = words("PATH=/system/bin")
	original, ok := EncodeRequest(spec, slot[:])
	if !ok {
		t.Fatal("encode")
	}
	for size := 0; size < len(original); size++ {
		if _, ok := DecodeRequest(original[:size]); ok {
			t.Fatalf("accepted truncation %d", size)
		}
	}
	if _, ok := DecodeRequest(append(append([]byte(nil), original...), 0)); ok {
		t.Fatal("accepted trailing byte")
	}
	pathLength := headerSize + 1 + 4 + len("PATH=/system/bin")
	for name, mutate := range map[string]func([]byte){
		"no stages":          func(d []byte) { d[0] = 0 },
		"too many stages":    func(d []byte) { d[0] = MaxStages + 1 },
		"bad error source":   func(d []byte) { d[1] = 255 },
		"error source count": func(d []byte) { d[1] = MaxStages },
		"flags":              func(d []byte) { d[2] = 2 },
		"reserved":           func(d []byte) { d[3] = 1 },
		"cwd length":         func(d []byte) { binary.LittleEndian.PutUint32(d[4:8], 0xffffffff) },
		"variable count":     func(d []byte) { binary.LittleEndian.PutUint32(d[8:12], 0xffffffff) },
		"relative cwd":       func(d []byte) { d[headerSize] = 'x' },
		"variable name":      func(d []byte) { d[headerSize+1+4] = '=' },
		"zero in variable":   func(d []byte) { d[headerSize+1+4+2] = 0 },
		"path length":        func(d []byte) { binary.LittleEndian.PutUint32(d[pathLength:], 0xfffffff0) },
		"argument count": func(d []byte) {
			binary.LittleEndian.PutUint32(d[pathLength+4+len("/system/bin/readline"):], 0x7fffffff)
		},
		"no arguments": func(d []byte) { binary.LittleEndian.PutUint32(d[pathLength+4+len("/system/bin/readline"):], 0) },
	} {
		data := append([]byte(nil), original...)
		mutate(data)
		if _, ok := DecodeRequest(data); ok {
			t.Errorf("accepted malformed request: %s", name)
		}
	}
}

func TestSubmitRoundTripAndBounds(t *testing.T) {
	message, ok := EncodeSubmit(Slots-1, SlotSize, 5)
	slot, size, decoded := DecodeSubmit(message)
	if !ok || !decoded || slot != Slots-1 || size != SlotSize {
		t.Fatalf("submit = %d %d %v %v", slot, size, ok, decoded)
	}
	for _, bad := range []struct {
		slot uint8
		size int
		corr uint64
	}{{Slots, 16, 1}, {0, SlotSize + 1, 1}, {0, 1, 1}, {0, 16, 0}} {
		if _, ok := EncodeSubmit(bad.slot, bad.size, bad.corr); ok {
			t.Fatalf("encoded %#v", bad)
		}
	}
	for _, mutate := range []func(*ipc.Message){
		func(m *ipc.Message) { m.Version-- },
		func(m *ipc.Message) { m.PayloadSize++ },
		func(m *ipc.Message) { m.Payload[0] = Slots },
		func(m *ipc.Message) { m.Payload[4] = 0xff },
		func(m *ipc.Message) { m.HandleCount = 1 },
		func(m *ipc.Message) { m.Correlation = 0 },
	} {
		corrupt := message
		mutate(&corrupt)
		if _, _, ok := DecodeSubmit(corrupt); ok {
			t.Fatal("accepted malformed submit")
		}
	}
}

// TestManyArguments covers what GST3's one-byte counts could not carry: the
// request is bounded by the slot's bytes, not by an argument count.
func TestManyArguments(t *testing.T) {
	spec := pipelineSpec()
	spec.Count = 2
	arguments := [][]byte{[]byte("echo")}
	for index := 0; index < 3000; index++ {
		arguments = append(arguments, []byte("argument"))
	}
	spec.Stages[0].Arguments = arguments
	spec.CWD = append([]byte("/"), bytes.Repeat([]byte{'d'}, limits.PathMax-1)...)
	var slot [SlotSize]byte
	data, ok := EncodeRequest(spec, slot[:])
	if !ok || len(data) <= ipc.MaxPayload {
		t.Fatalf("encoded %d bytes, ok=%v", len(data), ok)
	}
	request, ok := DecodeRequest(data)
	if !ok || request.Stages[0].Arguments.Count() != 3001 || len(request.CWD) != limits.PathMax {
		t.Fatal("many arguments rejected")
	}
	spec.Stages[1].Arguments = [][]byte{[]byte("cat"), bytes.Repeat([]byte{'a'}, SlotSize)}
	if _, ok := EncodeRequest(spec, slot[:]); ok {
		t.Fatal("a request larger than its slot encoded")
	}
}

func TestPipelineProtocolRejectsInvalidStage(t *testing.T) {
	for name, mutate := range map[string]func(*Spec){
		"no stages":          func(r *Spec) { r.Count = 0 },
		"too many stages":    func(r *Spec) { r.Count = MaxStages + 1 },
		"lone error source":  func(r *Spec) { r.Count, r.ErrorSource = 1, 1 },
		"error source range": func(r *Spec) { r.ErrorSource = MaxStages },
		"relative path":      func(r *Spec) { r.Stages[3].Path = []byte("tmp/readline") },
		"dot-dot path":       func(r *Spec) { r.Stages[3].Path = []byte("/tmp/../readline") },
		"no arguments":       func(r *Spec) { r.Stages[3].Arguments = nil },
		"empty argv0":        func(r *Spec) { r.Stages[3].Arguments = words("") },
		"zero byte":          func(r *Spec) { r.Stages[3].Arguments = words("x", "a\x00") },
		"long argument":      func(r *Spec) { r.Stages[3].Arguments = [][]byte{[]byte("x"), make([]byte, startup.MaxString+1)} },
		"bad variable":       func(r *Spec) { r.Environment = words("=value") },
		"variable no equals": func(r *Spec) { r.Environment = words("NAME") },
		"long path":          func(r *Spec) { r.Stages[0].Path = append([]byte("/"), bytes.Repeat([]byte{'p'}, limits.PathMax)...) },
	} {
		spec := pipelineSpec()
		mutate(&spec)
		var slot [SlotSize]byte
		if _, ok := EncodeRequest(spec, slot[:]); ok {
			t.Errorf("accepted invalid pipeline: %s", name)
		}
	}
}

// A stage may name a program anywhere in the namespace; PID 1 decides where it
// is read from and whether it may run (ADR 0097).
func TestProtocolCarriesPathsOutsideSystemBin(t *testing.T) {
	spec := pipelineSpec()
	spec.Stages[3].Path = []byte("/home/demo/readline")
	var slot [SlotSize]byte
	data, ok := EncodeRequest(spec, slot[:])
	if !ok {
		t.Fatal("refused a canonical path outside /system/bin")
	}
	decoded, ok := DecodeRequest(data)
	if !ok || string(decoded.Stages[3].Path) != "/home/demo/readline" {
		t.Fatalf("decoded %q, %v", decoded.Stages[3].Path, ok)
	}
}

func TestProtocolRejectsInvalidPathsAndResults(t *testing.T) {
	for _, path := range []string{"hello", "/system/../hello", "/system//hello", "/"} {
		var slot [SlotSize]byte
		if _, ok := EncodeRequest(Spec{Count: 1, Stages: [MaxStages]StageSpec{{Path: []byte(path), Arguments: words("x")}}, CWD: []byte("/")}, slot[:]); ok {
			t.Fatalf("accepted %q", path)
		}
	}
	if _, ok := EncodeReply(Reply{}, 1); ok {
		t.Fatal("accepted empty successful result")
	}
}

func TestResolveExecutableUsesBoundedSystemPath(t *testing.T) {
	var target [MaxPath]byte
	path, ok := ResolveExecutable([]byte("args"), &target)
	if !ok || string(path) != "/system/bin/args" {
		t.Fatalf("resolved path = %q", path)
	}
	command, ok := CommandFromSystemPath(path)
	if !ok || string(command) != "args" {
		t.Fatalf("system command = %q", command)
	}
	if _, ok := CommandFromSystemPath([]byte("/tmp/args")); ok {
		t.Fatal("accepted executable outside system search root")
	}
	for _, command := range [][]byte{nil, []byte("../init"), []byte("nested/tool"), []byte("bad name"), bytes.Repeat([]byte{'n'}, limits.NameMax+1)} {
		if _, ok := ResolveExecutable(command, &target); ok {
			t.Fatalf("accepted command %q", command)
		}
	}
}

func TestAreaInsideSharedWindow(t *testing.T) {
	if AreaAddress%vm.PageSize != 0 || AreaSize%vm.PageSize != 0 || AreaAddress < vm.UserSharedAddress || AreaAddress+AreaSize > vm.UserSharedLimit {
		t.Fatalf("exec area %#x outside [%#x, %#x)", AreaAddress, vm.UserSharedAddress, vm.UserSharedLimit)
	}
}

func TestReplyNamesTheFailedStage(t *testing.T) {
	message, ok := EncodeReply(Reply{LaunchStatus: 2, Stage: 2}, 9)
	if !ok {
		t.Fatal("failed-launch reply refused")
	}
	reply, ok := DecodeReply(message)
	if !ok || reply.LaunchStatus != 2 || reply.Stage != 2 {
		t.Fatalf("reply = %#v", reply)
	}
	if _, ok := EncodeReply(Reply{LaunchStatus: 2, Stage: MaxStages}, 9); ok {
		t.Fatal("stage beyond the pipeline encoded")
	}
	if _, ok := EncodeReply(Reply{Process: 3, Stage: 1}, 9); ok {
		t.Fatal("stage on a launched job encoded")
	}
	message.Payload[24] = MaxStages
	if _, ok := DecodeReply(message); ok {
		t.Fatal("stage beyond the pipeline decoded")
	}
}

func TestStartedCarriesTheInputHandle(t *testing.T) {
	message, ok := EncodeStarted(2, 0x100000009, 5)
	if !ok {
		t.Fatal("started refused")
	}
	slot, input, correlation, ok := DecodeStarted(message)
	if !ok || slot != 2 || input != 0x100000009 || correlation != 5 {
		t.Fatalf("started = %d %#x %d %v", slot, input, correlation, ok)
	}
	// The notice has to survive the IPC layer: version 8 carried the
	// correlation in a notification's header, which ipc.Encode refuses, and
	// every foreground job was left without its input.
	var wire [ipc.WireMaxSize]byte
	if size, encoded := ipc.Encode(message, wire[:]); !encoded {
		t.Fatal("started notice cannot be sent")
	} else if decoded, valid := ipc.Decode(wire[:size]); !valid || decoded.Correlation != 0 {
		t.Fatal("started notice does not round trip through the wire")
	}
	if _, ok := DecodeReply(message); ok {
		t.Fatal("started decoded as a reply")
	}
	for _, bad := range []struct {
		slot  uint8
		input uint64
	}{{Slots, 1}, {0, 0}} {
		if _, ok := EncodeStarted(bad.slot, handle.ID(bad.input), 5); ok {
			t.Fatalf("%+v encoded", bad)
		}
	}
	message.HandleCount = 0
	if _, _, _, ok := DecodeStarted(message); ok {
		t.Fatal("started without a handle decoded")
	}
	message.HandleCount = 1
	message.Payload[1] = 0
	if _, _, _, ok := DecodeStarted(message); ok {
		t.Fatal("started without a job correlation decoded")
	}
}
