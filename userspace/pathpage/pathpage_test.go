package pathpage

import (
	"bytes"
	"goos/internal/capability"
	"goos/kernel/handle"
	"goos/kernel/vfs"
	"testing"
)

type fakeMemory struct {
	next    handle.ID
	objects map[handle.ID][]byte
	retired []handle.ID
}

func (m *fakeMemory) create() (handle.ID, uint64, []byte, bool) {
	m.next++
	memory := make([]byte, 8192)
	m.objects[m.next] = memory
	return m.next, uint64(m.next) * 8192, memory, true
}
func (m *fakeMemory) duplicate(id handle.ID, _ capability.Rights) (handle.ID, bool) { return id, true }
func (m *fakeMemory) retire(id handle.ID, _, _ uint64)                              { m.retired = append(m.retired, id) }

func TestAbandonIsolatesLateServerWrites(t *testing.T) {
	io := &fakeMemory{objects: make(map[handle.ID][]byte)}
	p := pageState[*fakeMemory]{io: io}
	p.mu.Lock()
	oldRequest := vfs.Request{Operation: vfs.OperationReadDir, Path: bytes.Repeat([]byte("a"), 300), NameReturn: true}
	old, ok := p.prepare(&oldRequest)
	if !ok {
		t.Fatal("prepare old")
	}
	oldMapping := io.objects[old] // server retains this after timeout/death
	p.abandon()
	p.mu.Unlock()
	if p.object != 0 || p.memory != nil || len(io.retired) != 1 {
		t.Fatal("abandoned page still locally accessible")
	}
	// A deliberately late writer overlaps all subsequent requests. The race
	// detector catches accidental reuse even if the final name looks correct.
	start, done := make(chan struct{}), make(chan struct{})
	go func() {
		<-start
		for i := 0; i < 1000; i++ {
			copy(oldMapping, "late server write")
		}
		close(done)
	}()
	close(start)
	for i := 0; i < 1000; i++ {
		p.mu.Lock()
		request := vfs.Request{Operation: vfs.OperationReadDir, Path: bytes.Repeat([]byte("b"), 300), NameReturn: true}
		fresh, ok := p.prepare(&request)
		if !ok || fresh == old {
			t.Fatal("abandoned object reused")
		}
		copy(p.memory, "fresh name")
		name := append([]byte(nil), p.name(10)...)
		p.mu.Unlock()
		if string(name) != "fresh name" {
			t.Fatalf("name %q", name)
		}
	}
	<-done
	if p.name(-1) != nil || p.name(len(p.memory)+1) != nil {
		t.Fatal("invalid name size accepted")
	}
}
