package terminal

import (
	"fmt"
	"strconv"
	"sync"
	"testing"
)

// TestIndependentOutputsAndParsers runs one Output and one Parser per
// goroutine. Each keeps only its own state, so batches and decoded keys stay
// with the goroutine that made them.
func TestIndependentOutputsAndParsers(t *testing.T) {
	for _, workers := range []int{2, 4, 8} {
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			var group sync.WaitGroup
			for worker := 0; worker < workers; worker++ {
				group.Add(1)
				go func(worker int) {
					defer group.Done()
					var storage [64]byte
					var batches []string
					output := NewOutput(storage[:], func(batch []byte) { batches = append(batches, string(batch)) })
					var parser Parser
					for round := 0; round < 200; round++ {
						text := "w" + strconv.Itoa(worker) + "r" + strconv.Itoa(round)
						output.WriteString(text)
						output.Flush()
						for _, value := range []byte(text) {
							if event, ok := parser.Feed(value); !ok || event.Rune != rune(value) {
								t.Errorf("parser %d: %q decoded as %+v, %v", worker, value, event, ok)
							}
						}
					}
					if len(batches) != 200 {
						t.Errorf("%d batches, want 200", len(batches))
					}
					for round, batch := range batches {
						if want := "w" + strconv.Itoa(worker) + "r" + strconv.Itoa(round); batch != want {
							t.Errorf("batch %q, want %q", batch, want)
						}
					}
				}(worker)
			}
			group.Wait()
		})
	}
}
