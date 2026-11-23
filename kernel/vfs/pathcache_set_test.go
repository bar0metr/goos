package vfs

import (
	"testing"

	"goos/kernel/ramfs"
)

type cacheKey struct {
	parent ramfs.NodeID
	name   []byte
}

// collidingKeys returns n distinct (parent, name) keys whose componentHash
// selects the given set. Parents are scanned from 1 upward with a fixed short
// name, which is deterministic and independent of any random hash output.
func collidingKeys(set, n int) []cacheKey {
	keys := make([]cacheKey, 0, n)
	name := []byte("x")
	for parent := uint64(1); len(keys) < n && parent < 1<<22; parent++ {
		id := ramfs.NodeID(parent)
		if int(componentHash(id, name)&(cacheSets-1)) == set {
			keys = append(keys, cacheKey{parent: id, name: name})
		}
	}
	return keys
}

// cacheWays keys that hash to one set coexist, and one more evicts exactly one
// victim, leaving cacheWays of the cacheWays+1 present (ADR 0230, FIX3 §11).
func TestComponentCacheSetAssociativity(t *testing.T) {
	namespace := &Namespace{}
	backend := &ramfs.FileSystem{}
	keys := collidingKeys(0, cacheWays+1)
	if len(keys) != cacheWays+1 {
		t.Fatalf("only %d colliding keys found", len(keys))
	}
	for index := 0; index < cacheWays; index++ {
		namespace.remember(backend, keys[index].parent, ramfs.NodeID(index+1), keys[index].name, false)
	}
	for index := 0; index < cacheWays; index++ {
		if child, _, found := namespace.component(backend, keys[index].parent, keys[index].name); !found || child != ramfs.NodeID(index+1) {
			t.Fatalf("key %d not found after insert", index)
		}
	}
	namespace.remember(backend, keys[cacheWays].parent, ramfs.NodeID(cacheWays+1), keys[cacheWays].name, false)
	present := 0
	for index := 0; index < cacheWays+1; index++ {
		if _, _, found := namespace.component(backend, keys[index].parent, keys[index].name); found {
			present++
		}
	}
	if present != cacheWays {
		t.Fatalf("%d of %d keys present, want %d", present, cacheWays+1, cacheWays)
	}
	if _, _, found := namespace.component(backend, keys[cacheWays].parent, keys[cacheWays].name); !found {
		t.Fatal("newest key evicted itself")
	}
}

// The per-set second-chance clock spares a recently referenced way once and
// evicts an unreferenced way instead (ADR 0230, FIX3 §11).
func TestComponentCacheSecondChance(t *testing.T) {
	namespace := &Namespace{}
	backend := &ramfs.FileSystem{}
	keys := collidingKeys(0, cacheWays+1)
	if len(keys) != cacheWays+1 {
		t.Fatalf("only %d colliding keys found", len(keys))
	}
	for index := 0; index < cacheWays; index++ {
		namespace.remember(backend, keys[index].parent, ramfs.NodeID(index+1), keys[index].name, false)
	}
	if _, _, found := namespace.component(backend, keys[0].parent, keys[0].name); !found {
		t.Fatal("key 0 missing")
	}
	namespace.remember(backend, keys[cacheWays].parent, ramfs.NodeID(cacheWays+1), keys[cacheWays].name, false)
	if _, _, found := namespace.component(backend, keys[0].parent, keys[0].name); !found {
		t.Fatal("referenced key was not spared")
	}
	if _, _, found := namespace.component(backend, keys[1].parent, keys[1].name); found {
		t.Fatal("unreferenced key was not evicted")
	}
}

// Evicting inside one set never disturbs a key in a different set (ADR 0230).
func TestComponentCacheOtherSetUnaffected(t *testing.T) {
	namespace := &Namespace{}
	backend := &ramfs.FileSystem{}
	keys := collidingKeys(0, cacheWays+1)
	if len(keys) != cacheWays+1 {
		t.Fatalf("only %d colliding keys found", len(keys))
	}
	for index := 0; index < cacheWays; index++ {
		namespace.remember(backend, keys[index].parent, ramfs.NodeID(index+1), keys[index].name, false)
	}
	other := collidingKeys(1, 1)
	if len(other) != 1 {
		t.Fatal("no key found for the other set")
	}
	namespace.remember(backend, other[0].parent, 99, other[0].name, false)
	namespace.remember(backend, keys[cacheWays].parent, ramfs.NodeID(cacheWays+1), keys[cacheWays].name, false)
	if child, _, found := namespace.component(backend, other[0].parent, other[0].name); !found || child != 99 {
		t.Fatalf("other-set key lost: found=%v child=%d", found, child)
	}
}

// Backend, parent and generation are part of the exact key: a mismatch in any
// never produces a false hit (ADR 0230, FIX3 §11).
func TestComponentCacheKeyMismatchNoFalseHit(t *testing.T) {
	namespace := &Namespace{}
	first := &ramfs.FileSystem{}
	second := &ramfs.FileSystem{}
	keys := collidingKeys(0, 1)
	if len(keys) != 1 {
		t.Fatal("no colliding key found")
	}
	namespace.remember(first, keys[0].parent, 7, keys[0].name, false)
	if _, _, found := namespace.component(second, keys[0].parent, keys[0].name); found {
		t.Fatal("backend mismatch false hit")
	}
	pair := collidingKeys(0, 2)
	if len(pair) != 2 {
		t.Fatal("no second colliding key found")
	}
	if _, _, found := namespace.component(first, pair[1].parent, pair[1].name); found {
		t.Fatal("parent mismatch false hit")
	}
	namespace.invalidate()
	if _, _, found := namespace.component(first, keys[0].parent, keys[0].name); found {
		t.Fatal("generation mismatch false hit")
	}
}

// A component longer than maxCachedName is never cached (ADR 0230).
func TestComponentCacheLongNameUncached(t *testing.T) {
	namespace := &Namespace{}
	backend := &ramfs.FileSystem{}
	long := make([]byte, maxCachedName+1)
	for index := range long {
		long[index] = 'a'
	}
	namespace.remember(backend, 1, 2, long, false)
	if _, _, found := namespace.component(backend, 1, long); found {
		t.Fatal("long component cached")
	}
}

// A component lookup probes at most cacheWays entries per component, even when
// the set is full (ADR 0230, FIX3 §13).
func TestComponentCacheProbeBound(t *testing.T) {
	namespace := &Namespace{}
	backend := &ramfs.FileSystem{}
	keys := collidingKeys(0, cacheWays+1)
	if len(keys) != cacheWays+1 {
		t.Fatalf("only %d colliding keys found", len(keys))
	}
	for index := 0; index < cacheWays+1; index++ {
		namespace.remember(backend, keys[index].parent, ramfs.NodeID(index+1), keys[index].name, false)
	}
	before := namespace.AuthorityCounters()
	for index := 0; index < cacheWays+1; index++ {
		namespace.component(backend, keys[index].parent, keys[index].name)
	}
	after := namespace.AuthorityCounters()
	if probes := after.ComponentProbes - before.ComponentProbes; probes > uint64(cacheWays*(cacheWays+1)) {
		t.Fatalf("probes = %d, want <= %d", probes, cacheWays*(cacheWays+1))
	}
}
