package startup

import (
	"encoding/binary"
	"goos/internal/limits"
	"goos/kernel/handle"
	"strings"
	"testing"
)

type variable struct{ name, value string }

func build(t *testing.T, target []byte, arguments []string, environment []variable, handles []string, ids []handle.ID) ([]byte, bool) {
	t.Helper()
	writer := NewWriter(target, len(arguments), len(environment), len(handles))
	writer.Command([]byte("cat"))
	writer.CWD([]byte("/system"))
	for _, argument := range arguments {
		writer.Argument([]byte(argument))
	}
	for _, entry := range environment {
		writer.Variable([]byte(entry.name), []byte(entry.value))
	}
	for index, name := range handles {
		var id handle.ID
		if ids != nil {
			id = ids[index]
		}
		writer.Handle([]byte(name), id)
	}
	return writer.Finish()
}

func TestRoundTrip(t *testing.T) {
	var wire [MaxSize]byte
	ids := []handle.ID{0x100000001, 0x100000002, 0x100000003, 0x100000004}
	encoded, ok := build(t, wire[:], []string{"cat", "motd"}, []variable{{"PATH", "/system/bin"}, {"EMPTY", ""}}, []string{"in", "stdout", "stderr", "vfs"}, ids)
	if !ok {
		t.Fatal("encode failed")
	}
	if size, ok := Size(encoded[:HeaderSize]); !ok || size != len(encoded) {
		t.Fatalf("size = %d, %v; want %d", size, ok, len(encoded))
	}
	data, ok := Decode(encoded)
	if !ok {
		t.Fatal("decode failed")
	}
	argument, argumentFound := data.Arg(1)
	path, pathFound := data.Env("PATH")
	empty, emptyFound := data.Env("EMPTY")
	if string(data.Command()) != "cat" || string(data.CWD()) != "/system" || !argumentFound || string(argument) != "motd" || !pathFound || string(path) != "/system/bin" || !emptyFound || len(empty) != 0 {
		t.Fatalf("command %q cwd %q argument %q path %q", data.Command(), data.CWD(), argument, path)
	}
	if data.ArgCount() != 1 || data.Args() != 2 || (Data{}).ArgCount() != 0 || data.EnvCount() != 2 || data.HandleCount() != 4 {
		t.Fatalf("counts %d %d %d %d", data.ArgCount(), data.Args(), data.EnvCount(), data.HandleCount())
	}
	if _, found := data.Arg(2); found {
		t.Fatal("argument past the end")
	}
	if _, found := data.Env("MISSING"); found {
		t.Fatal("missing variable found")
	}
	for index, name := range []string{"in", "stdout", "stderr", "vfs"} {
		if id, found := data.Handle(name); !found || id != ids[index] {
			t.Fatalf("%s = %#x, %v", name, id, found)
		}
	}
	if (Data{}).Command() != nil || (Data{}).CWD() != nil {
		t.Fatal("empty data has a command")
	}
}

// TestManyArguments is the point of GST4: the bound is bytes, not a count.
func TestManyArguments(t *testing.T) {
	var wire [MaxSize]byte
	arguments := make([]string, 2000)
	for index := range arguments {
		arguments[index] = "argument"
	}
	environment := make([]variable, 100)
	for index := range environment {
		environment[index] = variable{name: "V" + strings.Repeat("X", index), value: "value"}
	}
	encoded, ok := build(t, wire[:], arguments, environment, []string{"stdout"}, []handle.ID{7})
	if !ok {
		t.Fatal("2000 arguments rejected")
	}
	data, ok := Decode(encoded)
	if !ok || data.Args() != 2000 || data.EnvCount() != 100 {
		t.Fatalf("decoded %v with %d arguments", ok, data.Args())
	}
	if last, _ := data.Arg(1999); string(last) != "argument" {
		t.Fatalf("last argument %q", last)
	}
	long := strings.Repeat("a", MaxString)
	if _, ok := build(t, wire[:], []string{"cat", long}, nil, nil, nil); !ok {
		t.Fatal("an argument of MaxString bytes rejected")
	}
	if _, ok := build(t, wire[:], []string{"cat", long + "a"}, nil, nil, nil); ok {
		t.Fatal("an argument past MaxString accepted")
	}
	if _, ok := build(t, wire[:], []string{"cat", long, long}, nil, nil, nil); ok {
		t.Fatal("an object past MaxSize accepted")
	}
	small := make([]byte, 64)
	if _, ok := build(t, small, []string{"cat", "motd"}, nil, nil, nil); ok {
		t.Fatal("an object larger than its target accepted")
	}
}

func TestBindHandles(t *testing.T) {
	var wire [MaxSize]byte
	template, ok := build(t, wire[:], []string{"cat"}, nil, []string{"stdout", "vfs"}, nil)
	if !ok {
		t.Fatal("template encode failed")
	}
	if _, ok := Decode(template); ok {
		t.Fatal("unbound template accepted as process startup")
	}
	if BindHandles(template, []handle.ID{4}) || BindHandles(template, []handle.ID{4, 0}) {
		t.Fatal("wrong bindings accepted")
	}
	if !BindHandles(template, []handle.ID{0x100000004, 0x100000009}) {
		t.Fatal("bind failed")
	}
	data, ok := Decode(template)
	if !ok {
		t.Fatal("bound startup rejected")
	}
	if stdout, _ := data.Handle("stdout"); stdout != 0x100000004 {
		t.Fatalf("stdout = %#x", stdout)
	}
	if BindHandles(template, []handle.ID{1, 2}) {
		t.Fatal("a bound object bound again")
	}
}

func TestWriterRejectsBadInput(t *testing.T) {
	var wire [MaxSize]byte
	cases := map[string]func() ([]byte, bool){
		"duplicate handle": func() ([]byte, bool) { return build(t, wire[:], []string{"cat"}, nil, []string{"vfs", "vfs"}, nil) },
		"duplicate variable": func() ([]byte, bool) {
			return build(t, wire[:], []string{"cat"}, []variable{{"A", "1"}, {"A", "2"}}, nil, nil)
		},
		"empty name":       func() ([]byte, bool) { return build(t, wire[:], []string{"cat"}, []variable{{"", "1"}}, nil, nil) },
		"name with equals": func() ([]byte, bool) { return build(t, wire[:], []string{"cat"}, []variable{{"A=B", "1"}}, nil, nil) },
		"empty argv0":      func() ([]byte, bool) { return build(t, wire[:], []string{""}, nil, nil, nil) },
		"no arguments":     func() ([]byte, bool) { return build(t, wire[:], nil, nil, nil, nil) },
		"zero byte":        func() ([]byte, bool) { return build(t, wire[:], []string{"cat", "a\x00b"}, nil, nil, nil) },
		"long handle name": func() ([]byte, bool) {
			return build(t, wire[:], []string{"cat"}, nil, []string{strings.Repeat("h", 64)}, nil)
		},
		"too many handles": func() ([]byte, bool) {
			return build(t, wire[:], []string{"cat"}, nil, make([]string, MaxHandles+1), nil)
		},
	}
	for name, encode := range cases {
		if _, ok := encode(); ok {
			t.Errorf("%s accepted", name)
		}
	}
	writer := NewWriter(wire[:], 1, 0, 0)
	writer.Command([]byte("cat"))
	writer.Argument([]byte("cat"))
	if _, ok := writer.Finish(); ok {
		t.Error("an object without a working directory accepted")
	}
	writer = NewWriter(wire[:], 1, 0, 0)
	writer.Command([]byte("cat"))
	writer.CWD([]byte("relative"))
	writer.Argument([]byte("cat"))
	if _, ok := writer.Finish(); ok {
		t.Error("a relative working directory accepted")
	}
	writer = NewWriter(wire[:], 1, 0, 0)
	writer.Command([]byte(strings.Repeat("c", limits.PathMax+1)))
	writer.CWD([]byte("/"))
	writer.Argument([]byte("cat"))
	if _, ok := writer.Finish(); ok {
		t.Error("a command past PathMax accepted")
	}
}

func TestDecodeRejectsMalformedObjects(t *testing.T) {
	var wire [MaxSize]byte
	encoded, ok := build(t, wire[:], []string{"cat", "motd"}, []variable{{"PATH", "/system/bin"}}, []string{"stdout"}, []handle.ID{3})
	if !ok {
		t.Fatal("encode failed")
	}
	valid := append([]byte(nil), encoded...)
	mutate := func(name string, change func(object []byte) []byte) {
		t.Helper()
		object := change(append([]byte(nil), valid...))
		if _, ok := Decode(object); ok {
			t.Errorf("%s accepted", name)
		}
	}
	mutate("magic", func(object []byte) []byte { object[0] = 'X'; return object })
	mutate("version", func(object []byte) []byte { object[4]++; return object })
	mutate("reserved", func(object []byte) []byte { object[31] = 1; return object })
	mutate("truncated", func(object []byte) []byte { return object[:len(object)-1] })
	mutate("size past MaxSize", func(object []byte) []byte {
		binary.LittleEndian.PutUint32(object[8:12], uint32(MaxSize+1))
		return object
	})
	mutate("huge argument count", func(object []byte) []byte {
		binary.LittleEndian.PutUint32(object[12:16], 0xffffffff)
		return object
	})
	mutate("zero argument count", func(object []byte) []byte {
		binary.LittleEndian.PutUint32(object[12:16], 0)
		return object
	})
	mutate("string area moved", func(object []byte) []byte {
		binary.LittleEndian.PutUint32(object[20:24], binary.LittleEndian.Uint32(object[20:24])+1)
		return object
	})
	mutate("string outside the object", func(object []byte) []byte {
		binary.LittleEndian.PutUint32(object[HeaderSize+2*entrySize+4:], 0xfffffff0)
		return object
	})
	mutate("string inside the tables", func(object []byte) []byte {
		binary.LittleEndian.PutUint32(object[HeaderSize:], HeaderSize)
		return object
	})
	mutate("unbound handle", func(object []byte) []byte {
		at := HeaderSize + 5*entrySize + 8
		binary.LittleEndian.PutUint64(object[at:], 0)
		return object
	})
	if _, ok := Size(valid[:HeaderSize-1]); ok {
		t.Error("a short header accepted")
	}
	// Trailing bytes after the declared size are ignored, as they are in the
	// kernel's copy of a staging slot.
	if _, ok := Decode(append(valid, 0xff)); !ok {
		t.Error("trailing bytes rejected")
	}
}

func FuzzDecode(f *testing.F) {
	var wire [MaxSize]byte
	writer := NewWriter(wire[:], 2, 1, 1)
	writer.Command([]byte("cat"))
	writer.CWD([]byte("/"))
	writer.Argument([]byte("cat"))
	writer.Argument([]byte("motd"))
	writer.Variable([]byte("PATH"), []byte("/system/bin"))
	writer.Handle([]byte("stdout"), 3)
	encoded, _ := writer.Finish()
	f.Add(append([]byte(nil), encoded...))
	f.Fuzz(func(t *testing.T, object []byte) {
		data, ok := Decode(object)
		if !ok {
			return
		}
		for index := 0; index < data.Args(); index++ {
			if _, ok := data.Arg(index); !ok {
				t.Fatal("a validated argument is unreadable")
			}
		}
		for index := 0; index < data.EnvCount(); index++ {
			if name, _ := data.EnvAt(index); len(name) == 0 {
				t.Fatal("a validated variable has no name")
			}
		}
	})
}
