package goroutine

import (
	"strconv"
	"testing"

	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/kernel/nativethread"
	"goos/kernel/sched"
	"goos/userspace/runtime/sys"
)

func TestThreadJoinClaimKeepsNativeStatus(t *testing.T) {
	for _, status := range []kcall.Status{kcall.StatusInvalidArgument, kcall.StatusAlreadyExists, kcall.StatusNotFound, kcall.StatusPermissionDenied, kcall.StatusResourceExhausted} {
		t.Run(strconv.Itoa(int(status)), func(t *testing.T) {
			ops := threadJoinOps{
				claim: func(thread uint64) (uint64, kcall.Status) {
					if thread != 9 {
						t.Fatalf("claim(%d)", thread)
					}
					return 0, status
				},
			}
			// No collect, wait or release may follow a failed claim.
			if code, got := joinThread(9, ops); code != 0 || got != status {
				t.Fatalf("join = %d, %v; want 0, %v", code, got, status)
			}
		})
	}
}

func TestThreadJoinParksOnTheEndThenCollects(t *testing.T) {
	for _, delayed := range []bool{false, true} {
		collects, waits, releases := 0, 0, 0
		ops := threadJoinOps{
			claim: func(uint64) (uint64, kcall.Status) { return 77, kcall.StatusOK },
			collect: func(thread, token uint64) (uint64, kcall.Status) {
				collects++
				if thread != 9 || token != 77 {
					t.Fatalf("collect(%d, %d)", thread, token)
				}
				if delayed && collects < 3 {
					return 0, kcall.StatusTimeout
				}
				return 42, kcall.StatusOK
			},
			release: func(thread, token uint64) kcall.Status {
				releases++
				if thread != 9 || token != 77 {
					t.Fatalf("release(%d, %d)", thread, token)
				}
				return kcall.StatusNotFound // successful collection consumed it
			},
			wait: func(interests *[kcall.WaitAnyMaxObjects]sys.Interest, count int, deadline int64) (uint64, kcall.Status) {
				waits++
				want := sys.Interest{Handle: handle.ID(kcall.ThreadEndInterest(9)), Events: kcall.WaitCompleted}
				if count != 1 || interests[0] != want || deadline != 0 {
					t.Fatalf("wait interest=%v count=%d deadline=%d", interests[0], count, deadline)
				}
				return 1, kcall.StatusOK
			},
		}
		wantCollects := 1
		if delayed {
			wantCollects = 3
		}
		if code, status := joinThread(9, ops); code != 42 || status != kcall.StatusOK || collects != wantCollects || waits != wantCollects-1 || releases != 1 {
			t.Fatalf("join = %d, %v; collects=%d waits=%d releases=%d", code, status, collects, waits, releases)
		}
	}
}

func TestThreadJoinReleasesOnWaitFailureAndUnwind(t *testing.T) {
	for _, panics := range []bool{false, true} {
		releases := 0
		ops := threadJoinOps{
			claim:   func(uint64) (uint64, kcall.Status) { return 77, kcall.StatusOK },
			collect: func(uint64, uint64) (uint64, kcall.Status) { return 0, kcall.StatusTimeout },
			release: func(thread, token uint64) kcall.Status {
				releases++
				if thread != 9 || token != 77 {
					t.Fatal("release lost its target or token")
				}
				return kcall.StatusOK
			},
			wait: func(*[kcall.WaitAnyMaxObjects]sys.Interest, int, int64) (uint64, kcall.Status) {
				if panics {
					panic("unwind the joining G")
				}
				return 0, kcall.StatusResourceExhausted
			},
		}
		func() {
			defer func() {
				if recovered := recover(); panics != (recovered != nil) {
					t.Fatalf("panic = %v, want panic=%v", recovered, panics)
				}
			}()
			if _, status := joinThread(9, ops); status != kcall.StatusResourceExhausted {
				t.Fatalf("wait failure = %v", status)
			}
		}()
		if releases != 1 {
			t.Fatalf("release count=%d", releases)
		}
	}
}

func TestThreadJoinMixedNativeJoinersAndMigration(t *testing.T) {
	var group nativethread.Group
	firstM, _ := sched.NewTaskID(0, 1)
	secondM, _ := sched.NewTaskID(1, 1)
	target, _ := sched.NewTaskID(2, 1)
	group.Adopt(firstM)
	slot, _ := group.Reserve()
	group.Commit(slot, target)
	currentM := firstM
	ops := threadJoinOps{
		claim: func(thread uint64) (uint64, kcall.Status) {
			token, status := group.ClaimJoin(currentM, sched.TaskID(thread))
			if status != nativethread.Waiting {
				t.Fatalf("claim = %v", status)
			}
			return token, kcall.StatusOK
		},
		collect: func(thread, token uint64) (uint64, kcall.Status) {
			code, status := group.CollectJoin(sched.TaskID(thread), token)
			if status == nativethread.Waiting {
				return 0, kcall.StatusTimeout
			}
			if status != nativethread.Joined {
				t.Fatalf("collect = %v", status)
			}
			return code, kcall.StatusOK
		},
		release: func(thread, token uint64) kcall.Status {
			if group.ReleaseJoin(sched.TaskID(thread), token) {
				t.Fatal("successful collection retained its claim")
			}
			return kcall.StatusNotFound
		},
		wait: func(*[kcall.WaitAnyMaxObjects]sys.Interest, int, int64) (uint64, kcall.Status) {
			if _, status := group.Join(secondM, target, true); status != nativethread.Busy {
				t.Fatal("legacy native join bypassed a parked G's claim")
			}
			group.Leave(target, 42)
			group.Ended(target, 0)
			if _, status := group.Join(secondM, target, false); status != nativethread.Busy {
				t.Fatal("legacy native poll stole the exit code")
			}
			if _, status := group.ClaimJoin(secondM, target); status != nativethread.Busy {
				t.Fatal("another G stole the exit code")
			}
			group.Forget(firstM)
			currentM = secondM
			return 1, kcall.StatusOK
		},
	}
	if code, status := joinThread(uint64(target), ops); code != 42 || status != kcall.StatusOK || currentM != secondM {
		t.Fatalf("migrated join = %d, %v", code, status)
	}
}
