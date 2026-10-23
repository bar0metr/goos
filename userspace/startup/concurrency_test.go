package startup

import (
	"fmt"
	"goos/kernel/handle"
	"strconv"
	"sync"
	"testing"
)

// TestDataConcurrentReaders reads one startup object from several goroutines
// while each also builds and decodes objects of its own. Data is a read-only
// view and a Writer belongs to its caller, so nothing crosses.
func TestDataConcurrentReaders(t *testing.T) {
	var shared [MaxSize]byte
	encoded, ok := build(t, shared[:], []string{"cat", "motd"}, []variable{{"PATH", "/system/bin"}}, []string{"in", "stdout"}, []handle.ID{3, 4})
	if !ok {
		t.Fatal("encode failed")
	}
	data, ok := Decode(encoded)
	if !ok {
		t.Fatal("decode failed")
	}
	for _, workers := range []int{2, 4, 8} {
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			var group sync.WaitGroup
			for worker := 0; worker < workers; worker++ {
				group.Add(1)
				go func(worker int) {
					defer group.Done()
					var own [1024]byte
					for round := 0; round < 200; round++ {
						if argument, _ := data.Arg(1); string(argument) != "motd" {
							t.Errorf("shared argument = %q", argument)
						}
						if id, found := data.Handle("stdout"); !found || id != 4 {
							t.Errorf("shared handle = %d, %v", id, found)
						}
						name := "w" + strconv.Itoa(worker) + "r" + strconv.Itoa(round)
						writer := NewWriter(own[:], 1, 0, 0)
						writer.Command([]byte("echo"))
						writer.CWD([]byte("/"))
						writer.Argument([]byte(name))
						object, ok := writer.Finish()
						if !ok {
							t.Error("own encode failed")
							continue
						}
						mine, ok := Decode(object)
						if argument, _ := mine.Arg(0); !ok || string(argument) != name {
							t.Errorf("own argument = %q, want %q", argument, name)
						}
					}
				}(worker)
			}
			group.Wait()
		})
	}
}
