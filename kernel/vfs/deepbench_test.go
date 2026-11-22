package vfs

import (
	"testing"

	"goos/kernel/ramfs"
)

// deepBenchRun mirrors fsbench's deep workload on a RAMFS namespace.
func deepBenchRun(b *testing.B) {
	backend := &ramfs.FileSystem{}
	namespace := &Namespace{}
	if namespace.Mount([]byte("/"), backend, backend.Init()) != StatusOK {
		b.Fatal("mount")
	}
	if namespace.Create([]byte("/fsbench.2"), true) != StatusOK {
		b.Fatal("base mkdir")
	}
	path := []byte("/fsbench.2")
	for level := 0; level < 100; level++ {
		path = append(path, '/', 'd')
		if namespace.Create(path, true) != StatusOK {
			b.Fatalf("mkdir level %d", level)
		}
	}
	leaf := append(append([]byte{}, path...), '/', 'l', 'e', 'a', 'f')
	if namespace.Create(leaf, false) != StatusOK {
		b.Fatal("leaf create")
	}
	if _, _, status := namespace.Resolve(leaf); status != StatusOK {
		b.Fatal("leaf stat")
	}
	if namespace.Unlink(leaf) != StatusOK {
		b.Fatal("leaf unlink")
	}
	for level := 0; level < 100; level++ {
		if namespace.Unlink(path) != StatusOK {
			b.Fatalf("rmdir level %d", level)
		}
		path = path[:len(path)-2]
	}
}

func BenchmarkDeepWorkload(b *testing.B) {
	for index := 0; index < b.N; index++ {
		deepBenchRun(b)
	}
}
