package supervisor

import (
	"goos/kernel/handle"
	execproto "goos/userspace/exec"
	"reflect"
	"testing"
)

type pipelineRunnerTest struct {
	t                                                  *testing.T
	count, errorSource, created, failCreate, failStage int
	owned                                              map[handle.ID]bool
	launched, waited                                   []int
	exits                                              [execproto.MaxStages]uint64
	background                                         bool
}

func (r *pipelineRunnerTest) CreateStream() (handle.ID, handle.ID, bool) {
	i := r.created
	r.created++
	if i == r.failCreate {
		return 0, 0, false
	}
	read, write := handle.ID(i*2+1), handle.ID(i*2+2)
	r.owned[read], r.owned[write] = true, true
	return read, write, true
}

func (r *pipelineRunnerTest) Close(id handle.ID) bool {
	if id == 99 {
		r.t.Fatal("the pipeline closed the terminal's shared writing end")
	}
	if !r.owned[id] {
		r.t.Fatalf("close of unowned endpoint %d", id)
	}
	delete(r.owned, id)
	return true
}

// chain returns the number of stages connected by stdout streams. The stderr
// stream, when present, is created after the chain streams.
func (r *pipelineRunnerTest) chain() int {
	if r.errorSource >= 0 {
		return r.count - 1
	}
	return r.count
}

func (r *pipelineRunnerTest) expect(name string, endpoint, want Endpoint) {
	if endpoint != want || want.Type == handle.TypeStream && !want.Shared && !r.owned[want.ID] {
		r.t.Fatalf("invalid %s endpoint %+v want %+v", name, endpoint, want)
	}
}

func (r *pipelineRunnerTest) Launch(stage execproto.Stage, cwd []byte, env execproto.List, streams Streams) (uint64, handle.ID, LaunchStatus) {
	i := int(stage.Path[0])
	r.launched = append(r.launched, i)
	if string(cwd) != "/system" || env.Count() != 0 {
		r.t.Fatal("startup context changed")
	}
	chain := r.chain()
	errRead, errWrite := handle.ID((chain-1)*2+1), handle.ID((chain-1)*2+2)
	// Whatever is not connected to a stream of the pipeline's own writes to a
	// copy of the terminal's writing end, which PID 1 keeps.
	terminal := Endpoint{ID: 99, Type: handle.TypeStream, Shared: true}
	input, output, failure := terminal, terminal, terminal
	if i >= chain {
		input = Endpoint{ID: errRead, Type: handle.TypeStream}
	} else {
		if i > 0 {
			input = Endpoint{ID: handle.ID((i-1)*2 + 1), Type: handle.TypeStream}
		} else {
			// The first stage reads its own input stream, created after the
			// chain and the error stream; a background job's is already at EOF.
			empty := chain - 1
			if r.errorSource >= 0 {
				empty++
			}
			input = Endpoint{ID: handle.ID(empty*2 + 1), Type: handle.TypeStream}
		}
		if i < chain-1 {
			output = Endpoint{ID: handle.ID(i*2 + 2), Type: handle.TypeStream}
		}
		if i == r.errorSource {
			failure = Endpoint{ID: errWrite, Type: handle.TypeStream}
		}
	}
	r.expect("input", streams.Input, input)
	r.expect("output", streams.Output, output)
	r.expect("error", streams.Error, failure)
	if i == r.failStage {
		return 0, 0, LaunchNotFound
	}
	for _, endpoint := range []Endpoint{streams.Input, streams.Output, streams.Error} {
		if endpoint.Type == handle.TypeStream && !endpoint.Shared {
			delete(r.owned, endpoint.ID)
		}
	}
	return uint64(i + 10), handle.ID(i + 20), LaunchOK
}

func (r *pipelineRunnerTest) Wait(process handle.ID) uint64 {
	if len(r.owned) != 0 {
		r.t.Fatal("wait before releasing retained endpoints")
	}
	i := int(process) - 20
	r.waited = append(r.waited, i)
	return r.exits[i]
}

func TestPipelineOwnershipAndFailures(t *testing.T) {
	for count := 1; count <= execproto.MaxStages; count++ {
		for failCreate := -1; failCreate < count-1; failCreate++ {
			for failStage := -1; failStage < count; failStage++ {
				r := &pipelineRunnerTest{t: t, count: count, errorSource: -1, failCreate: failCreate, failStage: failStage, owned: make(map[handle.ID]bool)}
				request := execproto.Request{Count: uint8(count), CWD: []byte("/system")}
				for i := 0; i < count; i++ {
					request.Stages[i].Path = []byte{byte(i)}
				}
				pid, status, code := RunPipeline(request, 99, r)
				if len(r.owned) != 0 {
					t.Fatal("leaked endpoints")
				}
				var launches, waits []int
				wantStatus := LaunchOK
				if failCreate >= 0 {
					wantStatus = LaunchResourceExhausted
				} else {
					for i := count - 1; i >= 0; i-- {
						launches = append(launches, i)
						if i == failStage {
							wantStatus = LaunchNotFound
							break
						}
					}
					for i := 0; i < count; i++ {
						if failStage < 0 || i > failStage {
							waits = append(waits, i)
						}
					}
				}
				if status != wantStatus || code != 0 || status != LaunchOK && pid != 0 || status == LaunchOK && pid != uint64(count+9) {
					t.Fatalf("result pid=%d status=%d code=%d", pid, status, code)
				}
				if !reflect.DeepEqual(r.launched, launches) || !reflect.DeepEqual(r.waited, waits) {
					t.Fatalf("launches=%v waits=%v want %v %v", r.launched, r.waited, launches, waits)
				}
			}
		}
	}
}

func TestPipelineReturnsFirstFailingStage(t *testing.T) {
	r := &pipelineRunnerTest{t: t, count: 4, errorSource: -1, failCreate: -1, failStage: -1, owned: make(map[handle.ID]bool), exits: [4]uint64{0, 7, 9, 0}}
	request := execproto.Request{Count: 4, CWD: []byte("/system")}
	for i := range request.Stages {
		request.Stages[i].Path = []byte{byte(i)}
	}
	_, status, code := RunPipeline(request, 99, r)
	if status != LaunchOK || code != 7 || len(r.waited) != 4 {
		t.Fatalf("status=%d exit=%d waited=%v", status, code, r.waited)
	}
}

func TestPipelineErrorSinkOwnershipAndFailures(t *testing.T) {
	for count := 2; count <= execproto.MaxStages; count++ {
		for source := 0; source < count-1; source++ {
			// count-2 chain streams plus one error stream.
			for failCreate := -1; failCreate < count-1; failCreate++ {
				for failStage := -1; failStage < count; failStage++ {
					r := &pipelineRunnerTest{t: t, count: count, errorSource: source, failCreate: failCreate, failStage: failStage, owned: make(map[handle.ID]bool)}
					request := execproto.Request{Count: uint8(count), ErrorSource: uint8(source + 1), CWD: []byte("/system")}
					for i := 0; i < count; i++ {
						request.Stages[i].Path = []byte{byte(i)}
					}
					pid, status, code := RunPipeline(request, 99, r)
					if len(r.owned) != 0 {
						t.Fatalf("leaked endpoints count=%d source=%d create=%d stage=%d", count, source, failCreate, failStage)
					}
					var launches, waits []int
					wantStatus := LaunchOK
					if failCreate >= 0 {
						wantStatus = LaunchResourceExhausted
					} else {
						for i := count - 1; i >= 0; i-- {
							launches = append(launches, i)
							if i == failStage {
								wantStatus = LaunchNotFound
								break
							}
						}
						for i := 0; i < count; i++ {
							if failStage < 0 || i > failStage {
								waits = append(waits, i)
							}
						}
					}
					// The reported pid belongs to the final stdout stage, not the sink.
					if status != wantStatus || code != 0 || status != LaunchOK && pid != 0 || status == LaunchOK && pid != uint64(count+8) {
						t.Fatalf("result pid=%d status=%d code=%d", pid, status, code)
					}
					if !reflect.DeepEqual(r.launched, launches) || !reflect.DeepEqual(r.waited, waits) {
						t.Fatalf("launches=%v waits=%v want %v %v", r.launched, r.waited, launches, waits)
					}
				}
			}
		}
	}
}

func TestPipelineRejectsInvalidErrorSource(t *testing.T) {
	for _, request := range []execproto.Request{
		{Count: 1, ErrorSource: 1},
		{Count: 3, ErrorSource: 3},
		{Count: 3, ErrorSource: 4},
	} {
		request.CWD = []byte("/system")
		r := &pipelineRunnerTest{t: t, errorSource: -1, failCreate: -1, failStage: -1, owned: make(map[handle.ID]bool)}
		if _, status, _ := RunPipeline(request, 99, r); status != LaunchInvalidImage || r.created != 0 || len(r.launched) != 0 {
			t.Fatalf("accepted %+v", request)
		}
	}
}

func TestBackgroundPipelineReadsEmptyInput(t *testing.T) {
	runner := &pipelineRunnerTest{t: t, count: 3, errorSource: -1, failCreate: -1, failStage: -1, owned: map[handle.ID]bool{}, background: true}
	runner.exits = [execproto.MaxStages]uint64{0, 4, 7}
	request := execproto.Request{Count: 3, Background: true, CWD: []byte("/system")}
	for index := range request.Stages[:3] {
		request.Stages[index].Path = []byte{byte(index)}
	}
	job := StartPipeline(request, 99, runner)
	if runner.created != 3 || len(runner.owned) != 0 {
		t.Fatalf("created %d streams, retained %v", runner.created, runner.owned)
	}
	var order []handle.ID
	for {
		process, pending := job.Next()
		if !pending {
			break
		}
		order = append(order, process)
		job.Complete(runner.Wait(process))
	}
	pid, status, code := job.Result()
	if !reflect.DeepEqual(order, []handle.ID{20, 21, 22}) || pid != 12 || status != LaunchOK || code != 4 {
		t.Fatalf("order %v pid %d status %d code %d", order, pid, status, code)
	}
}

func TestPipelineNamesTheStageThatFailedToLaunch(t *testing.T) {
	for failStage := 0; failStage < execproto.MaxStages; failStage++ {
		r := &pipelineRunnerTest{t: t, count: execproto.MaxStages, errorSource: -1, failCreate: -1, failStage: failStage, owned: make(map[handle.ID]bool)}
		request := execproto.Request{Count: execproto.MaxStages, CWD: []byte("/system")}
		for i := range request.Stages {
			request.Stages[i].Path = []byte{byte(i)}
		}
		job := StartPipeline(request, 99, r)
		for {
			process, pending := job.Next()
			if !pending {
				break
			}
			job.Complete(r.Wait(process))
		}
		if _, status, _ := job.Result(); status != LaunchNotFound || int(job.FailedStage()) != failStage {
			t.Fatalf("fail stage %d: status=%d stage=%d", failStage, status, job.FailedStage())
		}
	}
}

func TestLaunchStatusReasons(t *testing.T) {
	for status := LaunchOK; status <= LaunchCapabilityInstallFailed; status++ {
		if status.Reason() == "launch failed" {
			t.Fatalf("status %d has no reason", status)
		}
	}
	if LaunchNotFound.Reason() != "command not found" || !LaunchNotFound.NamesExecutable() || LaunchResourceExhausted.NamesExecutable() {
		t.Fatal("not-found launch is not described by the executable's name")
	}
}

func TestForegroundPipelineHandsOverItsInput(t *testing.T) {
	runner := &pipelineRunnerTest{t: t, count: 2, errorSource: -1, failCreate: -1, failStage: -1, owned: map[handle.ID]bool{}}
	request := execproto.Request{Count: 2, CWD: []byte("/system")}
	for index := range request.Stages[:2] {
		request.Stages[index].Path = []byte{byte(index)}
	}
	job := StartPipeline(request, 99, runner)
	input := job.TakeInput()
	// The chain stream comes first, then the input stream: its writer is the
	// only endpoint PID 1 still holds.
	if input == 0 || !runner.owned[input] || len(runner.owned) != 1 || runner.created != 2 {
		t.Fatalf("input %d, retained %v, created %d", input, runner.owned, runner.created)
	}
	if job.TakeInput() != 0 {
		t.Fatal("input handed over twice")
	}
	runner.Close(input)
	for {
		process, pending := job.Next()
		if !pending {
			break
		}
		job.Complete(runner.Wait(process))
	}
	if _, status, _ := job.Result(); status != LaunchOK {
		t.Fatalf("status %d", status)
	}
}

func TestBackgroundPipelineKeepsNoInput(t *testing.T) {
	runner := &pipelineRunnerTest{t: t, count: 1, errorSource: -1, failCreate: -1, failStage: -1, owned: map[handle.ID]bool{}, background: true}
	request := execproto.Request{Count: 1, Background: true, CWD: []byte("/system"), Stages: [execproto.MaxStages]execproto.Stage{{Path: []byte{0}}}}
	job := StartPipeline(request, 99, runner)
	if job.TakeInput() != 0 || len(runner.owned) != 0 {
		t.Fatalf("background job kept input: %v", runner.owned)
	}
}
