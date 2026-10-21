//go:build cryptoprobe_tls && !goos_userspace

package cryptoprobe

import (
	"crypto/rand"
	"sync"
	"testing"
)

func TestConcurrentProbesKeepProcessEntropyReader(t *testing.T) {
	original := rand.Reader
	var probes sync.WaitGroup
	for index := 0; index < 8; index++ {
		probes.Add(1)
		go func() {
			defer probes.Done()
			for round := 0; round < 4; round++ {
				if err := Run(); err == nil {
					t.Error("probe unexpectedly succeeded without native kernel or TLS peer")
				}
				if rand.Reader != original {
					t.Error("probe replaced the process entropy reader")
				}
			}
		}()
	}
	probes.Wait()
	if rand.Reader != original {
		t.Fatal("probe left a different entropy reader behind")
	}
}
