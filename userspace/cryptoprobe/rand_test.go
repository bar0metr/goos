//go:build cryptoprobe_rand && !goos_userspace

package cryptoprobe

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"sync"
	"testing"
)

func TestRandRequiresKernel(t *testing.T) {
	if err := Run(); err == nil {
		t.Fatal("host probe unexpectedly obtained kernel entropy")
	}
}

func TestRandProbeReadsItsOwnSource(t *testing.T) {
	original := rand.Reader
	var probes sync.WaitGroup
	for index := 0; index < 8; index++ {
		probes.Add(1)
		go func() {
			defer probes.Done()
			reader := bytes.NewReader(make([]byte, 32))
			if err := probeRandom(reader); err != nil || reader.Len() != 0 {
				t.Errorf("probe error=%v remaining=%d", err, reader.Len())
			}
			if rand.Reader != original {
				t.Error("probe replaced process entropy")
			}
		}()
	}
	probes.Wait()
	if err := probeRandom(bytes.NewReader([]byte{1})); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short source = %v", err)
	}
}
