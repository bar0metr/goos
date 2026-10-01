package supervisor

import "testing"

func TestBootStateMovesOnlyForward(t *testing.T) {
	var state BootState
	if state.Mode() != ModeStarting || state.Line() != "[MODE] starting\n" {
		t.Fatalf("initial state %v %q", state.Mode(), state.Line())
	}
	if !state.Normal() || state.Mode() != ModeNormal || state.Normal() {
		t.Fatal("starting did not enter normal exactly once")
	}
	if state.Line() != "[MODE] normal\n" {
		t.Fatalf("normal line %q", state.Line())
	}
	if !state.Recover(StageRuntime, "/system", "storage lost") {
		t.Fatal("normal did not enter recovery")
	}
	if state.Recover(StageRecord, "/system/release", "release mismatch") || state.Normal() {
		t.Fatal("recovery changed again")
	}
	if state.Stage() != StageRuntime || string(state.Reason()) != "storage lost" {
		t.Fatalf("recovery kept %v %q", state.Stage(), state.Reason())
	}
	if want := "[MODE] recovery: stage=runtime path=/system reason=storage lost\n"; state.Line() != want {
		t.Fatalf("recovery line %q", state.Line())
	}
}

func TestBootStateBoundsItsText(t *testing.T) {
	var state BootState
	long := string(make([]byte, 3*MaxBootText))
	state.Recover(StageForced, long, long)
	if len(state.Path()) != MaxBootText || len(state.Reason()) != MaxBootText {
		t.Fatalf("kept %d and %d bytes", len(state.Path()), len(state.Reason()))
	}
}

func TestTrustedIndexFollowsOriginAndMode(t *testing.T) {
	cases := []struct {
		path      string
		mode      Mode
		published bool
		want      IndexKind
	}{
		{"/bootstrap/bin/fsck-fat", ModeRecovery, false, IndexBootstrap},
		{"/bootstrap/bin/fsck-fat", ModeNormal, true, IndexBootstrap},
		{"/system/bin/fsck-fat", ModeNormal, true, IndexSystem},
		// Not while the installed system is withdrawn, nor in recovery.
		{"/system/bin/fsck-fat", ModeNormal, false, IndexNone},
		{"/system/bin/fsck-fat", ModeRecovery, true, IndexNone},
		// A copy of a system command, under any name, is described by none.
		{"/home/user/bin/fsck-fat", ModeNormal, true, IndexNone},
		{"/persist/system/bin/fsck-fat", ModeNormal, true, IndexNone},
		{"/apps/fsck-fat", ModeNormal, true, IndexNone},
		{"/bootstrap/fsck-fat", ModeNormal, true, IndexNone},
		{"/system/services/shell", ModeNormal, true, IndexNone},
	}
	for _, test := range cases {
		if got := TrustedIndex([]byte(test.path), test.mode, test.published); got != test.want {
			t.Errorf("TrustedIndex(%s, %v, %v) = %d, want %d", test.path, test.mode, test.published, got, test.want)
		}
	}
}

func TestServicesAndSearchFollowTheMode(t *testing.T) {
	if ServicePath("shell", ModeNormal) != "/system/services/shell" || ServicePath("shell", ModeRecovery) != "/bootstrap/shell" ||
		ServicePath("log", ModeStarting) != "/bootstrap/log" {
		t.Fatal("service paths do not follow the mode")
	}
	if SearchPath(ModeNormal) != "/system/bin:/home/user/bin" || SearchPath(ModeRecovery) != "/bootstrap/bin" {
		t.Fatal("search paths do not follow the mode")
	}
}
