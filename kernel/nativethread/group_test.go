package nativethread

import (
	"testing"

	"goos/kernel/sched"
)

func task(slot uint32, generation uint32) sched.TaskID {
	id, _ := sched.NewTaskID(slot, generation)
	return id
}

func TestCreateCommitAndCancel(t *testing.T) {
	var group Group
	group.Adopt(task(3, 1))
	if slot, ok := group.SlotOf(task(3, 1)); !ok || slot != 0 {
		t.Fatal("the main task is not slot 0")
	}
	slot, ok := group.Reserve()
	if !ok || slot != 1 {
		t.Fatalf("reserve %d %v", slot, ok)
	}
	if _, ok := group.SlotOf(task(4, 1)); ok {
		t.Fatal("a reserved slot names a task")
	}
	group.Cancel(slot)
	again, _ := group.Reserve()
	if again != slot || !group.Commit(again, task(4, 1)) || group.Commit(again, task(5, 1)) {
		t.Fatal("a cancelled slot was not reused, or a slot committed twice")
	}
	if group.Live() != 2 {
		t.Fatalf("%d live threads", group.Live())
	}
	for count := 2; count < Slots; count++ {
		if _, ok := group.Reserve(); !ok {
			t.Fatalf("slot %d not reserved", count)
		}
	}
	if _, ok := group.Reserve(); ok {
		t.Fatal("more slots than the group has")
	}
}

func TestJoinOrders(t *testing.T) {
	var group Group
	main, worker := task(1, 1), task(2, 1)
	group.Adopt(main)
	slot, _ := group.Reserve()
	group.Commit(slot, worker)

	if _, status := group.Join(worker, worker, true); status != Self {
		t.Fatal("a thread joined itself")
	}
	if _, status := group.Join(main, worker, false); status != Waiting {
		t.Fatal("a running thread was not waited for")
	}
	if _, status := group.Join(main, worker, true); status != Waiting {
		t.Fatal("join did not register")
	}
	if _, status := group.Join(task(9, 1), worker, true); status != Busy {
		t.Fatal("a second joiner was accepted")
	}
	if !group.Leave(worker, 7) {
		t.Fatal("leave")
	}
	ending, found := group.Ended(worker, 99)
	if !found || !ending.Leaving || ending.Code != 7 || ending.Joiner != main || ending.Slot != slot {
		t.Fatalf("ending %+v", ending)
	}
	if code, status := group.Join(main, worker, true); status != Joined || code != 7 {
		t.Fatalf("join %d %d", code, status)
	}
	if _, status := group.Join(main, worker, true); status != NotFound {
		t.Fatal("a thread was joined twice")
	}
	// The slot's next thread has a new generation; the old ID names
	// nothing.
	next, _ := group.Reserve()
	group.Commit(next, task(2, 2))
	if next != slot {
		t.Fatal("the slot was not reused")
	}
	if _, status := group.Join(main, worker, true); status != NotFound {
		t.Fatal("a stale ID named the slot's new thread")
	}
}

func TestEndThatIsNotLeavingKeepsItsCode(t *testing.T) {
	var group Group
	group.Adopt(task(1, 1))
	slot, _ := group.Reserve()
	group.Commit(slot, task(2, 1))
	ending, found := group.Ended(task(2, 1), 1<<63|14)
	if !found || ending.Leaving || ending.Code != 1<<63|14 {
		t.Fatalf("ending %+v", ending)
	}
	if !group.SetExitCode(ending.Code) || group.SetExitCode(0) {
		t.Fatal("the first exit code did not stick")
	}
	if code, set := group.ExitCode(); !set || code != 1<<63|14 {
		t.Fatal("exit code")
	}
	if ending, found := group.Ended(task(1, 1), 0); !found || ending.Slot != 0 {
		t.Fatal("the main thread did not end")
	}
	group.Adopt(task(7, 1))
	if _, ok := group.SlotOf(task(7, 1)); ok {
		t.Fatal("an ended group adopted another main task")
	}
}

func TestForgetDropsAJoiner(t *testing.T) {
	var group Group
	main, worker := task(1, 1), task(2, 1)
	group.Adopt(main)
	slot, _ := group.Reserve()
	group.Commit(slot, worker)
	group.Join(main, worker, true)
	group.Forget(main)
	if ending, _ := group.Ended(worker, 0); ending.Joiner != 0 {
		t.Fatal("a joiner that ended is still woken")
	}
}

func TestSuspensionIsExclusive(t *testing.T) {
	var group Group
	group.Adopt(task(1, 1))
	for index := uint32(2); index < 5; index++ {
		slot, _ := group.Reserve()
		group.Commit(slot, task(index, 1))
	}
	var others [Slots]sched.TaskID
	count := group.Others(task(1, 1), others[:])
	if count != 3 {
		t.Fatalf("%d others", count)
	}
	if !group.BeginSuspend(task(1, 1), others[:count]) || group.BeginSuspend(task(2, 1), others[:1]) {
		t.Fatal("two suspensions at once")
	}
	var held [Slots]sched.TaskID
	if _, ok := group.EndSuspend(task(2, 1), held[:]); ok {
		t.Fatal("a task ended a suspension it does not own")
	}
	if released, ok := group.EndSuspend(task(1, 1), held[:]); !ok || released != 3 || held[2] != task(4, 1) {
		t.Fatalf("end suspend %d %v", released, ok)
	}
	if group.Suspender() != 0 {
		t.Fatal("suspension still recorded")
	}
	group.Reset()
	if group.Live() != 0 {
		t.Fatal("reset left threads")
	}
}

func TestFindNamesJoinableThreadsByGeneration(t *testing.T) {
	var group Group
	group.Adopt(task(0, 1))
	slot, _ := group.Reserve()
	old := task(5, 1)
	if !group.Commit(slot, old) {
		t.Fatal("commit")
	}
	if _, found := group.Find(task(0, 1)); found {
		t.Fatal("the main thread is joinable")
	}
	if state, found := group.Find(old); !found || state != Running {
		t.Fatalf("running thread = %v, %v", state, found)
	}
	if _, found := group.Find(task(5, 2)); found {
		t.Fatal("another generation of the slot's task was found")
	}
	group.Leave(old, 7)
	group.Ended(old, 0)
	if state, found := group.Find(old); !found || state != Exited {
		t.Fatalf("ended thread = %v, %v", state, found)
	}
	if code, status := group.Join(task(0, 1), old, false); status != Joined || code != 7 {
		t.Fatalf("join = %d, %v", code, status)
	}
	if _, found := group.Find(old); found {
		t.Fatal("a joined thread is still found")
	}
	if _, found := group.Find(0); found {
		t.Fatal("task zero is found")
	}
}

func TestTokenJoinExcludesNativeAndOtherTokenJoiners(t *testing.T) {
	var group Group
	main, worker, otherM := task(0, 1), task(1, 1), task(2, 1)
	group.Adopt(main)
	slot, _ := group.Reserve()
	group.Commit(slot, worker)
	token, status := group.ClaimJoin(main, worker)
	if status != Waiting || token == 0 {
		t.Fatalf("claim = %d, %v", token, status)
	}
	for _, caller := range []sched.TaskID{main, otherM} {
		for _, register := range []bool{false, true} {
			if _, status := group.Join(caller, worker, register); status != Busy {
				t.Fatal("native join bypassed the token reservation")
			}
		}
		if _, status := group.ClaimJoin(caller, worker); status != Busy {
			t.Fatal("second token join bypassed the reservation")
		}
	}
	if _, status := group.CollectJoin(worker, token); status != Waiting {
		t.Fatal("a running target completed")
	}
	// Ending the source M does not revoke a G-owned reservation. Collection
	// on a different M needs only the same process, target and token.
	group.Forget(main)
	group.Leave(worker, 37)
	group.Ended(worker, 0)
	if _, status := group.Join(otherM, worker, false); status != Busy {
		t.Fatal("native join stole an ended token target")
	}
	if _, status := group.ClaimJoin(otherM, worker); status != Busy {
		t.Fatal("second token join stole an ended target")
	}
	if code, status := group.CollectJoin(worker, token); status != Joined || code != 37 {
		t.Fatalf("collect = %d, %v", code, status)
	}
	if group.ReleaseJoin(worker, token) {
		t.Fatal("collected reservation still exists")
	}
}

func TestNativeJoinExcludesTokensUntilCollectionOrDeath(t *testing.T) {
	var group Group
	main, worker, other := task(0, 1), task(1, 1), task(2, 1)
	group.Adopt(main)
	slot, _ := group.Reserve()
	group.Commit(slot, worker)
	group.Join(main, worker, true)
	if _, status := group.ClaimJoin(other, worker); status != Busy {
		t.Fatal("token claim bypassed a native joiner")
	}
	group.Leave(worker, 8)
	group.Ended(worker, 0)
	if _, status := group.ClaimJoin(other, worker); status != Busy {
		t.Fatal("token claim stole an ended native target")
	}
	if _, status := group.Join(other, worker, false); status != Busy {
		t.Fatal("native poll stole an ended native target")
	}
	group.Forget(main)
	token, status := group.ClaimJoin(other, worker)
	if status != Waiting {
		t.Fatal("dead native joiner left its reservation")
	}
	if code, status := group.CollectJoin(worker, token); status != Joined || code != 8 {
		t.Fatalf("collect = %d, %v", code, status)
	}
}

func TestJoinTokenReleaseAndGenerationChecks(t *testing.T) {
	var group Group
	main, worker := task(0, 1), task(1, 1)
	group.Adopt(main)
	slot, _ := group.Reserve()
	group.Commit(slot, worker)
	if _, status := group.ClaimJoin(worker, worker); status != Self {
		t.Fatal("self claim accepted")
	}
	for _, foreign := range []sched.TaskID{0, main, task(1, 2), task(9, 1)} {
		if _, status := group.ClaimJoin(main, foreign); status != NotFound && status != Self {
			t.Fatalf("foreign claim = %v", status)
		}
	}
	first, _ := group.ClaimJoin(main, worker)
	if group.ReleaseJoin(worker, 0) || group.ReleaseJoin(worker, first+1) || group.ReleaseJoin(task(1, 2), first) {
		t.Fatal("an invalid token or generation released a reservation")
	}
	if !group.ReleaseJoin(worker, first) {
		t.Fatal("release failed")
	}
	second, _ := group.ClaimJoin(main, worker)
	if second == first || second == 0 || group.ReleaseJoin(worker, first) {
		t.Fatal("a token was reused")
	}
	group.Leave(worker, 6)
	group.Ended(worker, 0)
	if _, status := group.CollectJoin(worker, first); status != NotFound {
		t.Fatal("old token collected a new reservation")
	}
	if !group.ReleaseJoin(worker, second) {
		t.Fatal("an ended target's reservation could not be released")
	}
	if code, status := group.Join(main, worker, false); status != Joined || code != 6 {
		t.Fatalf("native collection after release = %d, %v", code, status)
	}
	slot, _ = group.Reserve()
	group.Commit(slot, task(1, 2))
	third, _ := group.ClaimJoin(main, task(1, 2))
	if _, status := group.CollectJoin(worker, third); status != NotFound {
		t.Fatal("new token collected an old task generation")
	}
	group.Reset()
	if group.ReleaseJoin(task(1, 2), third) {
		t.Fatal("process teardown retained a reservation")
	}
}

func TestJoinTokenExhaustionDoesNotWrap(t *testing.T) {
	var group Group
	main, worker := task(0, 1), task(1, 1)
	group.Adopt(main)
	slot, _ := group.Reserve()
	group.Commit(slot, worker)
	group.joinSequence = ^uint64(0)
	if token, status := group.ClaimJoin(main, worker); token != 0 || status != Exhausted {
		t.Fatalf("exhausted claim = %d, %v", token, status)
	}
	if _, status := group.Join(main, worker, false); status != Waiting {
		t.Fatal("exhaustion left a reservation")
	}
}
