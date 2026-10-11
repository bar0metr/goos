package supervisor

import (
	"goos/kernel/handle"
	execproto "goos/userspace/exec"
	"testing"
)

type nativeStreamsRunner struct {
	t       *testing.T
	owned   map[handle.ID]bool
	stages  [execproto.MaxStages]Streams
	created int
	fail    bool
}

func (runner *nativeStreamsRunner) CreateStream() (handle.ID, handle.ID, bool) {
	runner.created++
	read, write := handle.ID(runner.created*2), handle.ID(runner.created*2+1)
	runner.owned[read] = true
	runner.owned[write] = true
	return read, write, true
}
func (runner *nativeStreamsRunner) Close(id handle.ID) bool {
	if !runner.owned[id] {
		runner.t.Fatalf("closed caller stream %d", id)
	}
	delete(runner.owned, id)
	return true
}
func (runner *nativeStreamsRunner) Launch(stage execproto.Stage, _ []byte, _ execproto.List, streams Streams) (uint64, handle.ID, LaunchStatus) {
	index := int(stage.Path[0])
	runner.stages[index] = streams
	if runner.fail && index == 0 {
		return 0, 0, LaunchNotFound
	}
	for _, endpoint := range []Endpoint{streams.Input, streams.Output, streams.Error} {
		if !endpoint.Shared {
			delete(runner.owned, endpoint.ID)
		}
	}
	return uint64(index + 10), handle.ID(index + 20), LaunchOK
}
func (*nativeStreamsRunner) Wait(handle.ID) uint64 { return 0 }
func TestPipelineNativeStreamsOwnershipAndStatus(t *testing.T) {
	standard := Streams{Input: Endpoint{90, handle.TypeStream, true}, Output: Endpoint{91, handle.TypeStream, true}, Error: Endpoint{92, handle.TypeStream, true}}
	for _, fail := range []bool{false, true} {
		runner := &nativeStreamsRunner{t: t, owned: map[handle.ID]bool{}, fail: fail}
		request := execproto.Request{Count: 2}
		request.Stages[0].Path = []byte{0}
		request.Stages[1].Path = []byte{1}
		job := StartPipelineStreams(request, standard, runner)
		if job.TakeInput() != 0 || runner.created != 1 {
			t.Fatal("native input replaced by tty input")
		}
		if len(runner.owned) != 0 {
			t.Fatal("leaked private stream", runner.owned)
		}
		if runner.stages[0].Input != standard.Input || runner.stages[0].Error != standard.Error || runner.stages[1].Output != standard.Output || runner.stages[1].Error != standard.Error {
			t.Fatal("native streams changed", runner.stages)
		}
		for {
			process, pending := job.Next()
			if !pending {
				break
			}
			if process == 20 {
				job.Complete(7)
			} else {
				job.Complete(9)
			}
		}
		_, status, code := job.Result()
		if fail {
			if status != LaunchNotFound {
				t.Fatal(status)
			}
		} else if status != LaunchOK || code != 7 {
			t.Fatal("first stage status lost", status, code)
		}
	}
}
