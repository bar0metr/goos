package abi

import (
	"reflect"
	"testing"
)

func TestFrameLayoutMatchesAssembly(t *testing.T) {
	frameType := reflect.TypeOf(Frame{})
	offsets := []struct {
		name  string
		field int
		want  uintptr
	}{
		{"number", 0, FrameNumberOffset},
		{"arg1", 1, FrameArg1Offset},
		{"arg2", 2, FrameArg2Offset},
		{"arg3", 3, FrameArg3Offset},
		{"arg4", 4, FrameArg4Offset},
		{"arg5", 5, FrameArg5Offset},
		{"arg6", 6, FrameArg6Offset},
		{"ip", 7, FrameUserIPOffset},
		{"flags", 8, FrameUserFlagsOffset},
		{"stack", 9, FrameUserStackOffset},
		{"root", 10, FrameRootOffset},
		{"return", 11, FrameReturnOffset},
		{"action", 12, FrameActionOffset},
	}
	for _, offset := range offsets {
		if got := frameType.Field(offset.field).Offset; got != offset.want {
			t.Fatalf("%s offset = %d, want %d", offset.name, got, offset.want)
		}
	}
	if frameType.Size() != FrameSize {
		t.Fatalf("frame size = %d, want %d", frameType.Size(), FrameSize)
	}
}

func TestNegativeErrno(t *testing.T) {
	if Negative(0) != 0 || int64(Negative(EPERM)) != -1 || int64(Negative(ENOSYS)) != -38 {
		t.Fatal("Linux errno encoding is wrong")
	}
}
