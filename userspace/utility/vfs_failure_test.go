package utility

import (
	"testing"

	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/vfs"
)

func TestVFSFailureCapturesOutcomeBeforeQuarantine(t *testing.T) {
	for _, test := range []struct {
		name, phase    string
		raw            kcall.Status
		sent, received bool
	}{
		{"send", "send", kcall.StatusResourceExhausted, false, false},
		{"timeout", "receive", kcall.StatusTimeout, true, false},
		{"peer-death", "receive", kcall.StatusPeerDied, true, false},
		{"wire", "wire", kcall.StatusCorruptData, true, false},
		{"correlation", "reply", kcall.StatusCorruptData, true, false},
		{"completed-error", "complete", kcall.StatusOK, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := newScript()
			service.reply = func(request ipc.Message) ipc.Message { return replyTo(request, nil, nil) }
			switch test.name {
			case "send":
				service.sendStatus = test.raw
			case "timeout", "peer-death":
				service.status = test.raw
			case "wire":
				service.raw = []byte{255}
			case "correlation":
				service.reply = func(request ipc.Message) ipc.Message { request.Correlation++; return replyTo(request, nil, nil) }
			case "completed-error":
				service.reply = func(request ipc.Message) ipc.Message {
					message, _ := vfs.EncodeReply(vfs.Reply{Operation: vfs.OperationStat, Status: vfs.StatusBackendError}, request.Correlation)
					return message
				}
			}
			s := newOwned(service, 10)
			failure := VFSFailure{Phase: "stale", ReplyCorrelation: 999}
			_, received := s.callObserved(7, vfs.Request{Operation: vfs.OperationStat}, 0, 44, 5, &failure)
			if received != test.received || failure.Phase != test.phase || failure.Status != test.raw || failure.Sent != test.sent {
				t.Fatalf("received=%v failure=%+v", received, failure)
			}
			if test.name == "correlation" {
				if failure.ReplyCorrelation != 45 || failure.ReplyOperation != vfs.OperationStat {
					t.Fatalf("lost rejected reply: %+v", failure)
				}
			} else if failure.ReplyCorrelation != 0 {
				t.Fatal("snapshot retained an earlier reply")
			}
			if received {
				if s.bound() != 7 || len(s.quarantined) != 0 {
					t.Fatal("completed error was quarantined")
				}
			} else {
				if s.bound() != 0 || len(s.quarantined) != 1 {
					t.Fatal("failed endpoint not quarantined")
				}
				count := len(service.sent)
				_, received = s.callObserved(7, vfs.Request{Operation: vfs.OperationStat}, 0, 45, 5, &failure)
				if received || failure.Phase != "bound" || failure.Status != kcall.StatusPeerDied || failure.Sent || len(service.sent) != count {
					t.Fatal("diagnostics revived quarantined endpoint")
				}
			}
		})
	}
}

func TestVFSFailureDistinguishesUnsentAndSentDeadline(t *testing.T) {
	for _, sent := range []bool{false, true} {
		service := newScript()
		s := newOwned(service, 10, 15)
		if sent {
			s.io.clock.ticks = []uint64{10, 11, 15}
		}
		var failure VFSFailure
		_, received := s.callObserved(7, vfs.Request{Operation: vfs.OperationStat}, 0, 44, 5, &failure)
		if received || failure.Phase != "deadline" || failure.Status != kcall.StatusTimeout || failure.Sent != sent {
			t.Fatalf("sent=%v failure=%+v", sent, failure)
		}
		if (len(s.quarantined) != 0) != sent {
			t.Fatal("diagnostics changed unsent/sent deadline policy")
		}
	}
}
