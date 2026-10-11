package supervisor

import "testing"

func TestShutdownGraceTargetsOnlyRequesterAndExpires(t *testing.T) {
	var grace ShutdownGrace
	if !grace.Begin(42, 100, 5) || grace.Ready(100, true) || grace.Ready(104, true) {
		t.Fatal("requester was not given its close window")
	}
	if !grace.Ready(101, false) || !grace.Ready(105, true) {
		t.Fatal("requester close or deadline did not release shutdown")
	}
	if remaining, active := grace.Remaining(102); !active || remaining != 3 {
		t.Fatalf("remaining=%d active=%t", remaining, active)
	}
	if remaining, active := grace.Remaining(105); !active || remaining != 1 {
		t.Fatalf("expired remaining=%d active=%t", remaining, active)
	}
	if grace.Begin(7, 102, 5) {
		t.Fatal("second shutdown replaced requester")
	}
}

func TestShutdownGraceRejectsInvalidDeadline(t *testing.T) {
	for _, test := range []struct{ session, now, timeout uint64 }{{0, 1, 5}, {1, 1, 0}, {1, ^uint64(0), 2}} {
		var grace ShutdownGrace
		if grace.Begin(test.session, test.now, test.timeout) || grace != (ShutdownGrace{}) {
			t.Fatalf("accepted invalid grace %+v", test)
		}
	}
}
