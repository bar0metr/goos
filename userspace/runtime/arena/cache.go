package arena

// threadCache is the heap a thread holds for itself. With the heap lock
// alone it holds nothing; the thread-owned slabs come in its place.
type threadCache struct{}

func (c *threadCache) reset() {}

// threadCounts is what a thread counted without the heap lock.
type threadCounts struct{}

// returnCache gives a thread's cache back to the heap. The caller holds the
// heap lock.
func (h *heap) returnCache(c *threadCache) {}

// foldCounts adds a thread's counts to the heap's. The caller holds the
// heap lock.
func (h *heap) foldCounts(c *threadCounts) {}
