package linux

import "testing"

func TestTerminationFromCode(t *testing.T) {
	cases := []struct {
		code   uint64
		status uint8
		signal int32
		wait   uint32
	}{
		{0, 0, 0, 0},
		{23, 23, 0, 23 << 8},
		{0x1ff, 0xff, 0, 0xff << 8},
		{abnormalExit | 14, 0, 11, 11},
		{abnormalExit | 13, 0, 11, 11},
		{abnormalExit | 6, 0, 4, 4},
		{abnormalExit | 0, 0, 8, 8},
		{abnormalExit | 3, 0, 5, 5},
		{abnormalExit | 17, 0, 7, 7},
		{abnormalExit | 9, 0, 9, 9},
		{abnormalExit | 5, 0, 9, 9},
		{SignalExitCode(15), 0, 15, 15},
		{SignalExitCode(13), 0, 13, 13},
	}
	for _, test := range cases {
		termination := TerminationFromCode(test.code, 12)
		record := ProcessRecord{ExitStatus: termination.Status, SignalTermination: termination.Signal}
		if termination.Status != test.status || termination.Signal != test.signal || termination.CPUTicks != 12 || record.WaitStatus() != test.wait {
			t.Fatalf("code %#x = %+v, wait status %#x", test.code, termination, record.WaitStatus())
		}
	}
}
