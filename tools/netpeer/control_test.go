package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedBuffer collects log lines written from the control goroutine.
type lockedBuffer struct {
	mu   sync.Mutex
	text strings.Builder
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.text.Write(data)
}

func (b *lockedBuffer) count(line string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Count(b.text.String(), line)
}

// The gate writes each command with its own open, write and close, often
// back to back. Every one must arrive and none may fail with EPIPE.
func TestControlFIFOTakesBackToBackWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control")
	if err := prepareControl(path); err != nil {
		t.Fatal(err)
	}
	var log lockedBuffer
	s := &server{host: testHost(t, &log), quit: make(chan struct{}), ports: map[*port]struct{}{}}
	go s.control(path)
	const writers = 5000
	for index := 0; index < writers; index++ {
		file, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteString("edns on\n"); err != nil {
			t.Fatalf("writer %d: %v", index, err)
		}
		file.Close()
	}
	deadline := time.Now().Add(5 * time.Second)
	for log.count("edns on\n") < writers && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := log.count("edns on\n"); got != writers {
		t.Fatalf("%d of %d commands arrived", got, writers)
	}
}
