package supervisor

import (
	"goos/kernel/handle"
	execproto "goos/userspace/exec"
	"reflect"
	"testing"
)

type pipelineRunnerTest struct {
	t                                     *testing.T
	count, created, failCreate, failStage int
	owned                                 map[handle.ID]bool
	launched, waited                      []int
	exits                                 [execproto.MaxStages]uint64
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
	if !r.owned[id] {
		r.t.Fatalf("close of unowned endpoint %d", id)
	}
	delete(r.owned, id)
	return true
}

func (r *pipelineRunnerTest) Launch(stage execproto.Stage, cwd, env []byte, input handle.ID, inputType handle.Type, output handle.ID, outputType handle.Type) (uint64, handle.ID, LaunchStatus) {
	i := int(stage.Path[0])
	r.launched = append(r.launched, i)
	if string(cwd) != "/system" || !reflect.DeepEqual(env, []byte{0}) {
		r.t.Fatal("startup context changed")
	}
	if i == 0 {
		if input != 99 || inputType != handle.TypeDevice {
			r.t.Fatal("invalid console input")
		}
	} else if input != handle.ID((i-1)*2+1) || inputType != handle.TypeStream || !r.owned[input] {
		r.t.Fatal("invalid stream input")
	}
	if i == r.count-1 {
		if output != 99 || outputType != handle.TypeDevice {
			r.t.Fatal("invalid console output")
		}
	} else if output != handle.ID(i*2+2) || outputType != handle.TypeStream || !r.owned[output] {
		r.t.Fatal("invalid stream output")
	}
	if i == r.failStage {
		return 0, 0, LaunchNotFound
	}
	if inputType == handle.TypeStream {
		delete(r.owned, input)
	}
	if outputType == handle.TypeStream {
		delete(r.owned, output)
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
				r := &pipelineRunnerTest{t: t, count: count, failCreate: failCreate, failStage: failStage, owned: make(map[handle.ID]bool)}
				request := execproto.Request{Count: uint8(count), CWD: []byte("/system"), Environment: []byte{0}}
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
	r := &pipelineRunnerTest{t: t, count: 4, failCreate: -1, failStage: -1, owned: make(map[handle.ID]bool), exits: [4]uint64{0, 7, 9, 0}}
	request := execproto.Request{Count: 4, CWD: []byte("/system"), Environment: []byte{0}}
	for i := range request.Stages {
		request.Stages[i].Path = []byte{byte(i)}
	}
	_, status, code := RunPipeline(request, 99, r)
	if status != LaunchOK || code != 7 || len(r.waited) != 4 {
		t.Fatalf("status=%d exit=%d waited=%v", status, code, r.waited)
	}
}
