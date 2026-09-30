package abi

import (
	"encoding/binary"
	"testing"

	"goos/kernel/compat/linux/elf"
	"goos/kernel/mm/vm"
	"goos/userspace/startup"
)

func testStartup(t testing.TB) startup.Data {
	t.Helper()
	storage := make([]byte, startup.MaxSize)
	writer := startup.NewWriter(storage, 3, 2, 0)
	writer.Command([]byte("/system/linux/probe"))
	writer.CWD([]byte("/tmp"))
	writer.Argument([]byte("probe"))
	writer.Argument([]byte("one"))
	writer.Argument([]byte("two"))
	writer.Variable([]byte("LANG"), []byte("C"))
	writer.Variable([]byte("EMPTY"), []byte{})
	object, ok := writer.Finish()
	if !ok {
		t.Fatal("startup build failed")
	}
	data, ok := startup.Decode(object)
	if !ok {
		t.Fatal("startup decode failed")
	}
	return data
}

func testImage() *elf.Image {
	return &elf.Image{Entry: 0x401020, ProgramHeaderAddress: 0x400040, ProgramHeaderCount: 7}
}

func stackOffset(t *testing.T, stack InitialStack, address uint64) int {
	t.Helper()
	if address < stack.Pointer || address >= stack.Top {
		t.Fatalf("address %#x outside [%#x, %#x)", address, stack.Pointer, stack.Top)
	}
	return int(address - stack.Pointer)
}

func stackString(t *testing.T, stack InitialStack, address uint64) string {
	t.Helper()
	start := stackOffset(t, stack, address)
	for end := start; end < len(stack.Data); end++ {
		if stack.Data[end] == 0 {
			return string(stack.Data[start:end])
		}
	}
	t.Fatal("unterminated stack string")
	return ""
}

func TestBuildInitialStackLayout(t *testing.T) {
	var target [MaxInitialStackSize]byte
	stack, failure := BuildInitialStack(target[:], vm.UserStackTop, testImage(), testStartup(t), nil)
	if failure != StackOK || stack.Pointer%16 != 0 || stack.Top != vm.UserStackTop || len(stack.Data) != int(stack.Top-stack.Pointer) {
		t.Fatalf("stack failure=%d layout=%+v", failure, stack)
	}
	word := func(offset int) uint64 { return binary.LittleEndian.Uint64(stack.Data[offset:]) }
	if word(0) != 3 {
		t.Fatalf("argc = %d", word(0))
	}
	argv := []string{"probe", "one", "two"}
	for index, want := range argv {
		if got := stackString(t, stack, word(8+index*8)); got != want {
			t.Errorf("argv[%d] = %q, want %q", index, got, want)
		}
	}
	if word(8+len(argv)*8) != 0 {
		t.Fatal("argv is not NULL terminated")
	}
	envpAt := 8 + (len(argv)+1)*8
	for index, want := range []string{"LANG=C", "EMPTY="} {
		if got := stackString(t, stack, word(envpAt+index*8)); got != want {
			t.Errorf("envp[%d] = %q, want %q", index, got, want)
		}
	}
	if word(envpAt+2*8) != 0 {
		t.Fatal("envp is not NULL terminated")
	}
	auxvAt := envpAt + 3*8
	auxv := make(map[uint64]uint64)
	for at := auxvAt; ; at += 16 {
		typeValue, value := word(at), word(at+8)
		auxv[typeValue] = value
		if typeValue == auxNull {
			break
		}
	}
	want := map[uint64]uint64{
		auxPHDR: 0x400040, auxPHENT: elf.ProgramHeaderSize, auxPHNUM: 7,
		auxPageSize: vm.PageSize, auxEntry: 0x401020,
		auxUID: SyntheticUID, auxEUID: SyntheticUID, auxGID: SyntheticGID, auxEGID: SyntheticGID,
		auxSecure: 0, auxClockTick: ClockTicks,
	}
	for typeValue, value := range want {
		if auxv[typeValue] != value {
			t.Errorf("auxv[%d] = %#x, want %#x", typeValue, auxv[typeValue], value)
		}
	}
	if got := stackString(t, stack, auxv[auxExecFN]); got != "/system/linux/probe" {
		t.Errorf("AT_EXECFN = %q", got)
	}
	if got := stackString(t, stack, auxv[auxPlatform]); got != "x86_64" {
		t.Errorf("AT_PLATFORM = %q", got)
	}
	if _, present := auxv[25]; present {
		t.Fatal("AT_RANDOM must be absent without trustworthy entropy")
	}
}

func TestBuildInitialStackCarriesRandomBytes(t *testing.T) {
	var target, plain [MaxInitialStackSize]byte
	random := [RandomSize]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	stack, failure := BuildInitialStack(target[:], vm.UserStackTop, testImage(), testStartup(t), &random)
	without, _ := BuildInitialStack(plain[:], vm.UserStackTop, testImage(), testStartup(t), nil)
	if failure != StackOK || stack.Pointer%16 != 0 || stack.Pointer >= without.Pointer {
		t.Fatalf("failure=%d pointer=%#x, without %#x", failure, stack.Pointer, without.Pointer)
	}
	word := func(offset int) uint64 { return binary.LittleEndian.Uint64(stack.Data[offset:]) }
	auxvAt := 8 + 4*8 + 3*8
	var address, entries uint64
	terminated := false
	for at := auxvAt; at+16 <= len(stack.Data); at += 16 {
		entries++
		if word(at) == auxRandom {
			address = word(at + 8)
		}
		if word(at) == auxNull {
			terminated = true
			break
		}
	}
	if !terminated || entries != auxiliaryCount+1 {
		t.Fatalf("auxv has %d entries, terminated=%v", entries, terminated)
	}
	at := stackOffset(t, stack, address)
	if at+RandomSize > len(stack.Data) || string(stack.Data[at:at+RandomSize]) != string(random[:]) {
		t.Fatalf("AT_RANDOM at %#x does not hold the bytes", address)
	}
	// The strings are where the pointers say, random block or not.
	if got := stackString(t, stack, word(8)); got != "probe" {
		t.Fatalf("argv[0] = %q", got)
	}
}

type badStartup struct {
	args int
	envs int
	arg  []byte
}

func (input badStartup) Command() []byte            { return []byte("/bad") }
func (input badStartup) Args() int                  { return input.args }
func (input badStartup) Arg(int) ([]byte, bool)     { return input.arg, true }
func (input badStartup) EnvCount() int              { return input.envs }
func (input badStartup) EnvAt(int) ([]byte, []byte) { return []byte("A"), []byte("B") }

func TestBuildInitialStackRejectsInvalidAndOversizedInput(t *testing.T) {
	var target [MaxInitialStackSize]byte
	tests := []struct {
		name  string
		input Startup
		want  StackError
	}{
		{name: "too many arguments", input: badStartup{args: MaxArguments + 1}, want: StackTooManyArguments},
		{name: "too much environment", input: badStartup{args: 1, arg: []byte("x"), envs: MaxEnvironment + 1}, want: StackTooManyEnvironment},
		{name: "NUL argument", input: badStartup{args: 1, arg: []byte{'x', 0}}, want: StackStringInvalid},
	}
	for _, test := range tests {
		if _, failure := BuildInitialStack(target[:], vm.UserStackTop, testImage(), test.input, nil); failure != test.want {
			t.Errorf("%s: failure %d, want %d", test.name, failure, test.want)
		}
	}
	if _, failure := BuildInitialStack(target[:128], vm.UserStackTop, testImage(), testStartup(t), nil); failure != StackTooLarge {
		t.Fatalf("small target failure = %d", failure)
	}
}

func TestBuildInitialStackAcceptsEmptyArgumentVector(t *testing.T) {
	var target [MaxInitialStackSize]byte
	stack, failure := BuildInitialStack(target[:], vm.UserStackTop, testImage(), badStartup{}, nil)
	if failure != StackOK || binary.LittleEndian.Uint64(stack.Data[:8]) != 0 {
		t.Fatalf("failure=%d argc=%d", failure, binary.LittleEndian.Uint64(stack.Data[:8]))
	}
}

type rawExecStartup struct{ badStartup }

func (rawExecStartup) Environment(int) ([]byte, bool) { return []byte("without-equals"), true }

func TestBuildInitialStackPreservesRawEnvironment(t *testing.T) {
	var target [MaxInitialStackSize]byte
	input := rawExecStartup{badStartup{args: 1, envs: 1, arg: []byte{}}}
	stack, failure := BuildInitialStack(target[:], vm.UserStackTop, testImage(), input, nil)
	if failure != StackOK {
		t.Fatalf("failure = %d", failure)
	}
	word := func(offset int) uint64 { return binary.LittleEndian.Uint64(stack.Data[offset:]) }
	if got := stackString(t, stack, word(24)); got != "without-equals" {
		t.Fatalf("envp[0] = %q", got)
	}
}

func FuzzBuildInitialStack(fuzz *testing.F) {
	fuzz.Add([]byte("arg"), []byte("value"))
	fuzz.Add([]byte{'x', 0}, []byte{})
	fuzz.Fuzz(func(t *testing.T, argument, value []byte) {
		if len(argument) > MaxInitialStackSize || len(value) > MaxInitialStackSize {
			t.Skip()
		}
		input := fuzzStartup{argument: argument, value: value}
		var target [MaxInitialStackSize]byte
		// Both shapes of the stack are built: with the random block and
		// without it.
		random := [RandomSize]byte{0xa5}
		for _, source := range []*[RandomSize]byte{nil, &random} {
			stack, failure := BuildInitialStack(target[:], vm.UserStackTop, testImage(), input, source)
			if failure == StackOK && (stack.Pointer%16 != 0 || stack.Pointer < vm.UserStackTop-elf.DefaultStackReserve || stack.Top != vm.UserStackTop) {
				t.Fatalf("invalid successful layout: %+v", stack)
			}
		}
	})
}

type fuzzStartup struct {
	argument []byte
	value    []byte
}

func (input fuzzStartup) Command() []byte            { return []byte("/probe") }
func (input fuzzStartup) Args() int                  { return 1 }
func (input fuzzStartup) Arg(int) ([]byte, bool)     { return input.argument, true }
func (input fuzzStartup) EnvCount() int              { return 1 }
func (input fuzzStartup) EnvAt(int) ([]byte, []byte) { return []byte("VALUE"), input.value }
