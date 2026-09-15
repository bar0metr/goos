package shmem

import (
	"goos/internal/capability"
	"goos/kernel/handle"
	"testing"
)

var sharedBenchmarkSink byte

func TestMappingBounds(t *testing.T) {
	tests := []struct {
		offset uint16
		count  uint16
		valid  bool
	}{
		{offset: 0, count: 1, valid: true},
		{offset: 1, count: 1, valid: true},
		{offset: 0, count: 2, valid: true},
		{offset: 0, count: 0},
		{offset: 2, count: 1},
		{offset: 1, count: 2},
	}
	for _, test := range tests {
		if got := validMapping(2, test.offset, test.count); got != test.valid {
			t.Errorf("validMapping(2, %d, %d) = %v", test.offset, test.count, got)
		}
	}
}

func TestGrantRejectsExecuteAndInvalidObject(t *testing.T) {
	var table handle.Table
	object := Object{id: 9, count: 1}
	if _, ok := object.Grant(&table, capability.Read|capability.Write); !ok {
		t.Fatal("valid shared-memory grant rejected")
	}
	if _, ok := object.Grant(&table, capability.Execute); ok {
		t.Fatal("executable shared-memory grant accepted")
	}
	if _, ok := (&Object{}).Grant(&table, capability.Read); ok {
		t.Fatal("invalid shared-memory object granted")
	}
}

func TestObjectMetadata(t *testing.T) {
	object := Object{id: 11, count: 3}
	if object.ID() != 11 || object.PageCount() != 3 {
		t.Fatalf("unexpected metadata: id=%d pages=%d", object.ID(), object.PageCount())
	}
}

func TestRegistryAllocatesUniqueNonzeroIdentities(t *testing.T) {
	var registry Registry
	first, firstOK := registry.allocateID()
	second, secondOK := registry.allocateID()
	if !firstOK || !secondOK || first == 0 || second == 0 || first == second {
		t.Fatalf("invalid identities: %d %d", first, second)
	}
	if first&identityNamespace == 0 || second&identityNamespace == 0 {
		t.Fatal("shared-memory identity lacks namespace bit")
	}
	registry.nextID = identityLimit
	if _, ok := registry.allocateID(); ok {
		t.Fatal("registry identity overflow accepted")
	}
}

func BenchmarkSharedPagePayloadAccess(b *testing.B) {
	var page [4096]byte
	producer := page[:]
	consumer := page[:]
	b.SetBytes(int64(len(page)))
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		for index := range producer {
			producer[index]++
			sharedBenchmarkSink ^= consumer[index]
		}
	}
}
