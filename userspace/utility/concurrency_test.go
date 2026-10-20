package utility

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// workerCounts are the goroutine counts each concurrency test runs with;
// host goroutines stand in for native threads.
var workerCounts = []int{2, 4, 8}

// echoService answers every request with a message carrying the request's
// correlation and payload. Replies queue per channel, as a native channel's
// do, so a caller that waits for a reply takes whichever is first.
type echoService struct {
	mu      sync.Mutex
	replies map[handle.ID][][]byte
}

type echoIPC struct{ service *echoService }

func (echoIPC) close(handle.ID) {}

func (e echoIPC) send(channel handle.ID, wire []byte) kcall.Status {
	request, ok := ipc.Decode(wire)
	if !ok {
		return kcall.StatusInvalidArgument
	}
	var reply [ipc.WireMaxSize]byte
	size, ok := ipc.Encode(request, reply[:])
	if !ok {
		return kcall.StatusInvalidArgument
	}
	e.service.mu.Lock()
	e.service.replies[channel] = append(e.service.replies[channel], append([]byte(nil), reply[:size]...))
	e.service.mu.Unlock()
	return kcall.StatusOK
}

func (e echoIPC) waitReceive(channel handle.ID, wire []byte, ticks uint64) kcall.Status {
	// Give another goroutine the chance to send in between.
	runtime.Gosched()
	e.service.mu.Lock()
	defer e.service.mu.Unlock()
	queue := e.service.replies[channel]
	if len(queue) == 0 {
		return kcall.StatusTimeout
	}
	copy(wire, queue[0])
	e.service.replies[channel] = queue[1:]
	return kcall.StatusOK
}

// TestSessionExchangesDoNotCross runs request and reply exchanges on one
// session from several goroutines. Each one must get the reply to its own
// request: the session's lock keeps one request outstanding at a time.
func TestSessionExchangesDoNotCross(t *testing.T) {
	for _, workers := range workerCounts {
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			service := &echoService{replies: map[handle.ID][][]byte{}}
			var s session[echoIPC]
			s.io = echoIPC{service}
			s.bind(7)
			const rounds = 300
			seen := make([]map[uint64]bool, workers)
			var group sync.WaitGroup
			for worker := 0; worker < workers; worker++ {
				seen[worker] = map[uint64]bool{}
				group.Add(1)
				go func(worker int) {
					defer group.Done()
					for round := 0; round < rounds; round++ {
						correlation := s.begin()
						s.outgoing = ipc.Message{ProtocolID: 1, Version: 1, Kind: ipc.KindRequest, Correlation: correlation, PayloadSize: 16}
						binary.LittleEndian.PutUint64(s.outgoing.Payload[0:], uint64(worker))
						binary.LittleEndian.PutUint64(s.outgoing.Payload[8:], uint64(round))
						if !s.send(&s.outgoing) {
							t.Error("send failed")
						}
						if !s.received(1) {
							t.Error("no reply")
						} else if s.incoming.Correlation != correlation ||
							binary.LittleEndian.Uint64(s.incoming.Payload[0:]) != uint64(worker) ||
							binary.LittleEndian.Uint64(s.incoming.Payload[8:]) != uint64(round) {
							t.Errorf("worker %d round %d got the reply to correlation %d", worker, round, s.incoming.Correlation)
						}
						s.end()
						if seen[worker][correlation] {
							t.Errorf("correlation %d handed out twice", correlation)
						}
						seen[worker][correlation] = true
					}
				}(worker)
			}
			group.Wait()
			all := map[uint64]bool{}
			for _, worker := range seen {
				for correlation := range worker {
					if all[correlation] {
						t.Fatalf("correlation %d used by two workers", correlation)
					}
					all[correlation] = true
				}
			}
			if len(all) != workers*rounds {
				t.Fatalf("%d distinct correlations, want %d", len(all), workers*rounds)
			}
		})
	}
}

func TestSessionReceiveRejectsOversizedHeader(t *testing.T) {
	var s session[oversizedIPC]
	s.bind(1)
	s.hold()
	defer s.end()
	status, valid := s.receive(1)
	if status != kcall.StatusOK || valid {
		t.Fatalf("receive = %v, %v; want OK and not valid", status, valid)
	}
}

// oversizedIPC delivers a header that claims more payload than a wire holds.
type oversizedIPC struct{}

func (oversizedIPC) close(handle.ID) {}

func (oversizedIPC) send(handle.ID, []byte) kcall.Status { return kcall.StatusOK }

func (oversizedIPC) waitReceive(_ handle.ID, wire []byte, _ uint64) kcall.Status {
	wire[24], wire[25] = 0xff, 0xff
	return kcall.StatusOK
}

// chunkSink takes at most a few bytes per write, so every record longer than
// that needs several writes, and yields between writes so other goroutines
// get the chance to cut in.
type chunkSink struct {
	mu      *sync.Mutex
	written *[]byte
	targets *[]uint64
}

func (c chunkSink) write(target uint64, value []byte) int {
	size := len(value)
	if size > 7 {
		size = 7
	}
	c.mu.Lock()
	*c.written = append(*c.written, value[:size]...)
	for range value[:size] {
		*c.targets = append(*c.targets, target)
	}
	c.mu.Unlock()
	runtime.Gosched()
	return size
}

// TestOutputRecordsDoNotInterleave writes records of many lengths to
// standard output and standard error from several goroutines through a
// stream that moves seven bytes per write. Every record must come out whole
// and on the stream it was written to; their order is free.
func TestOutputRecordsDoNotInterleave(t *testing.T) {
	for _, workers := range workerCounts {
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			var mu sync.Mutex
			var written []byte
			var targets []uint64
			var o output[chunkSink]
			o.io = chunkSink{&mu, &written, &targets}
			o.stdout, o.stderr = 1, 2
			const rounds = 60
			want := map[string]uint64{}
			for worker := 0; worker < workers; worker++ {
				for round := 0; round < rounds; round++ {
					want[record(worker, round)] = uint64(1 + round%2)
				}
			}
			var group sync.WaitGroup
			for worker := 0; worker < workers; worker++ {
				group.Add(1)
				go func(worker int) {
					defer group.Done()
					for round := 0; round < rounds; round++ {
						target := o.stdout
						if round%2 == 1 {
							target = o.errorTarget()
						}
						o.writeRecord(target, []byte(record(worker, round)))
					}
				}(worker)
			}
			group.Wait()
			got := 0
			offset := 0
			for _, line := range strings.SplitAfter(string(written), "\n") {
				if line == "" {
					continue
				}
				target, ok := want[line]
				if !ok {
					t.Fatalf("record cut or merged: %q", line)
				}
				for index := range line {
					if targets[offset+index] != target {
						t.Fatalf("record %q went to stream %d", line, targets[offset+index])
					}
				}
				offset += len(line)
				delete(want, line)
				got++
			}
			if len(want) != 0 || got != workers*rounds {
				t.Fatalf("%d records out, %d missing", got, len(want))
			}
		})
	}
}

// record is a distinct record of a length that depends on round, up to a few
// hundred bytes.
func record(worker, round int) string {
	return fmt.Sprintf("w%d r%d %s\n", worker, round, strings.Repeat(string(rune('a'+worker)), (round*37)%300))
}

func TestErrorTargetFallsBackToStdout(t *testing.T) {
	o := output[chunkSink]{stdout: 5}
	if o.errorTarget() != 5 {
		t.Fatalf("errorTarget = %d without standard error, want standard output", o.errorTarget())
	}
	o.stderr = 6
	if o.errorTarget() != 6 {
		t.Fatalf("errorTarget = %d, want 6", o.errorTarget())
	}
}

// TestAppendFailureConcurrent builds diagnostics in buffers each goroutine
// owns, the way a Line or a failing goroutine does, and checks their text.
func TestAppendFailureConcurrent(t *testing.T) {
	for _, workers := range workerCounts {
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			var group sync.WaitGroup
			for worker := 0; worker < workers; worker++ {
				group.Add(1)
				go func(worker int) {
					defer group.Done()
					var buffer [64]byte
					for round := 0; round < 200; round++ {
						path := []byte(fmt.Sprintf("/d%d/f%d\x1b", worker, round))
						got := appendFailure(buffer[:], []byte("cat"), path, "not found")
						want := fmt.Sprintf("cat: /d%d/f%d?: not found\n", worker, round)
						if string(got) != want {
							t.Errorf("appendFailure = %q, want %q", got, want)
						}
					}
				}(worker)
			}
			group.Wait()
		})
	}
}

func TestAppendFailureTruncatesAndKeepsNewline(t *testing.T) {
	var buffer [16]byte
	got := appendFailure(buffer[:], []byte("command"), nil, "a reason that does not fit")
	if len(got) != len(buffer) || got[len(got)-1] != '\n' || !bytes.HasPrefix(got, []byte("command: a reas")) {
		t.Fatalf("appendFailure = %q", got)
	}
	got = appendUsage(buffer[:], "x [-y] file and more")
	if len(got) != len(buffer) || got[len(got)-1] != '\n' || !bytes.HasPrefix(got, []byte("usage: x [-y] f")) {
		t.Fatalf("appendUsage = %q", got)
	}
}
