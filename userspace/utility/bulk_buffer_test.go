package utility

import (
	"bytes"
	"goos/internal/capability"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/vfs"
	"testing"
)

type bulkService struct {
	next    handle.ID
	objects map[handle.ID][]byte
	retired []handle.ID
	fail    bool
	last    []byte
}

func (s *bulkService) create(size uint64) (handle.ID, uint64, []byte, bool) {
	s.next++
	memory := make([]byte, int(size))
	s.objects[s.next] = memory
	return s.next, uint64(s.next) * 4096, memory, true
}
func (s *bulkService) duplicate(id handle.ID, _ capability.Rights) (handle.ID, bool) { return id, true }
func (s *bulkService) retire(id handle.ID, _, _ uint64)                              { s.retired = append(s.retired, id) }
func (s *bulkService) call(request vfs.Request, attached handle.ID) (vfs.Reply, bool) {
	s.last = s.objects[attached]
	if s.fail {
		return vfs.Reply{}, false
	}
	if request.Operation == vfs.OperationReadInto {
		copy(s.last, "fresh")
	}
	return vfs.Reply{Operation: request.Operation, Status: vfs.StatusOK, Size: uint64(request.Size)}, true
}

func TestBulkLateAccessCannotTouchCallerMemory(t *testing.T) {
	for _, operation := range []vfs.Operation{vfs.OperationReadInto, vfs.OperationWriteFrom} {
		s := &bulkService{objects: make(map[handle.ID][]byte), fail: true}
		b := bulkBuffer[*bulkService]{Memory: []byte("owner"), io: s}
		if _, ok := b.transfer(operation, 1, 5); ok {
			t.Fatal("timeout accepted")
		}
		old := s.last
		if !bytes.Equal(b.Memory, []byte("owner")) || b.object != 0 || len(s.retired) != 1 {
			t.Fatal("failed transfer changed caller memory or retained shared object")
		}
		start, done := make(chan struct{}), make(chan struct{})
		go func() {
			<-start
			for i := 0; i < 1000; i++ {
				if operation == vfs.OperationReadInto {
					copy(old, "late!")
				} else {
					_ = bytes.Equal(old, []byte("owner"))
				}
			}
			close(done)
		}()
		close(start)
		s.fail = false
		for i := 0; i < 1000; i++ {
			copy(b.Memory, "next!")
			if _, ok := b.transfer(operation, 2, 5); !ok {
				t.Fatal("fresh transfer failed")
			}
			want := "next!"
			if operation == vfs.OperationReadInto {
				want = "fresh"
			}
			if string(b.Memory) != want {
				t.Fatalf("caller memory = %q", b.Memory)
			}
		}
		<-done
		if operation == vfs.OperationWriteFrom && string(old) != "owner" {
			t.Fatal("late server read observed new write payload")
		}
	}
}

type bulkSessionTransport struct {
	*bulkService
	files *vfsSession[scriptedIPC, *testPaths]
}

func (s bulkSessionTransport) call(request vfs.Request, attached handle.ID) (vfs.Reply, bool) {
	return s.files.callAttached(request, attached, 1, 1)
}

func TestBulkTimeoutQuarantineAndFreshVFSRebind(t *testing.T) {
	service := newScript()
	service.status = kcall.StatusTimeout
	files := newFiles(service)
	memory := &bulkService{objects: make(map[handle.ID][]byte)}
	b := bulkBuffer[bulkSessionTransport]{Memory: []byte("owner"), io: bulkSessionTransport{memory, files}}
	if _, ok := b.transfer(vfs.OperationReadInto, 1, 5); ok || files.bound() != 0 || len(memory.retired) != 1 {
		t.Fatal("bulk timeout did not quarantine session and staging")
	}
	old := memory.objects[service.last[7].Handles[0]]
	if _, ok := b.transfer(vfs.OperationReadInto, 1, 5); ok || len(service.sent) != 1 {
		t.Fatal("next bulk transfer sent on abandoned endpoint")
	}
	files.bind(8)
	service.status = kcall.StatusOK
	service.reply = func(m ipc.Message) ipc.Message {
		copy(memory.objects[m.Handles[0]], "fresh")
		reply, _ := vfs.EncodeReply(vfs.Reply{Operation: vfs.OperationReadInto, Status: vfs.StatusOK, Size: 5}, m.Correlation)
		return reply
	}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			copy(old, "late!")
		}
		close(done)
	}()
	if _, ok := b.transfer(vfs.OperationReadInto, 2, 5); !ok || string(b.Memory) != "fresh" {
		t.Fatalf("fresh channel failed: %v, %q", ok, b.Memory)
	}
	<-done
	if string(b.Memory) != "fresh" {
		t.Fatal("late write changed fresh bulk reply")
	}
}
