package fsproto

import (
	"fmt"
	"goos/kernel/ipc"
	"goos/kernel/ramfs"
	"strconv"
	"sync"
	"testing"
)

// TestCodecConcurrentCallerBuffers encodes and decodes requests and replies
// from several goroutines at once, each with its own message and name buffer.
// The codec keeps no state of its own, so every round trip is intact.
func TestCodecConcurrentCallerBuffers(t *testing.T) {
	for _, workers := range []int{2, 4, 8} {
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			var group sync.WaitGroup
			for worker := 0; worker < workers; worker++ {
				group.Add(1)
				go func(worker int) {
					defer group.Done()
					var message ipc.Message
					var name [ramfs.MaxName]byte
					for round := 0; round < 300; round++ {
						text := []byte("w" + strconv.Itoa(worker) + "-" + strconv.Itoa(round))
						request := Request{Operation: OperationOpen, Parent: ramfs.NodeID(worker + 1), Name: text}
						if !EncodeRequest(request, uint64(round+1), &message, nil) {
							t.Error("encode request")
							return
						}
						decoded, ok := DecodeRequest(&message, nil, &name)
						if !ok || decoded.Parent != request.Parent || string(decoded.Name) != string(text) {
							t.Errorf("request round trip = %+v, %v", decoded, ok)
						}
						reply := Reply{Operation: OperationOpen, Status: ramfs.StatusOK, Kind: ramfs.KindFile, Node: ramfs.NodeID(round + 1), Name: text}
						if !EncodeReply(reply, uint64(round+1), &message, nil) {
							t.Error("encode reply")
							return
						}
						back, ok := DecodeReply(&message, nil, &name)
						if !ok || back.Node != reply.Node || string(back.Name) != string(text) {
							t.Errorf("reply round trip = %+v, %v", back, ok)
						}
					}
				}(worker)
			}
			group.Wait()
		})
	}
}
