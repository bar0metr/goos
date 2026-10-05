package vfs

import (
	"testing"

	"goos/kernel/ramfs"
)

func TestMetadataRequestRoundTrip(t *testing.T) {
	modified := ramfs.Timestamp{Seconds: 1_600_000_000, Nanoseconds: 9, Available: true}
	for _, change := range []ramfs.MetadataChange{
		{SetModified: true, Modified: modified, SetReadOnly: true, ReadOnly: true},
		{SetReadOnly: true},
		{SetModified: true, Modified: modified, SetReadOnly: true},
		{SetModified: true, Modified: modified},
	} {
		message, ok := EncodeRequest(MetadataRequest([]byte("/tmp/f"), 0, change), 1)
		if !ok {
			t.Fatalf("%+v: encode refused", change)
		}
		request, ok := DecodeRequest(message)
		if !ok {
			t.Fatalf("%+v: decode refused", change)
		}
		decoded, valid := MetadataChangeOf(request)
		if !valid || decoded != change || string(request.Path) != "/tmp/f" {
			t.Fatalf("%+v: decoded %+v %v", change, decoded, valid)
		}
	}
	// The metadata flags belong to one operation; elsewhere they are no right.
	message, _ := EncodeRequest(Request{Operation: OperationOpen, Path: []byte("/f"), Rights: MetadataWritable}, 1)
	if _, ok := DecodeRequest(message); ok {
		t.Fatal("an open with a metadata flag decoded")
	}
	if _, valid := MetadataChangeOf(Request{Operation: OperationSetMetadata, Rights: MetadataReadOnly | MetadataWritable}); valid {
		t.Fatal("read-only and writable at once accepted")
	}
	if _, valid := MetadataChangeOf(Request{Operation: OperationSetMetadata}); valid {
		t.Fatal("empty change accepted")
	}
}

func TestNamespaceSetMetadata(t *testing.T) {
	var namespace Namespace
	backend := &ramfs.FileSystem{}
	root := backend.Init()
	if namespace.Mount([]byte("/"), backend, root) != StatusOK {
		t.Fatal("mount refused")
	}
	if status := namespace.Create([]byte("/f"), false); status != StatusOK {
		t.Fatal(status)
	}
	change := ramfs.MetadataChange{SetReadOnly: true, ReadOnly: true}
	if status := namespace.SetMetadata([]byte("/f"), change); status != StatusOK {
		t.Fatal(status)
	}
	if status := namespace.SetMetadata([]byte("/missing"), change); status != StatusNotFound {
		t.Fatalf("missing file: %d", status)
	}
	stat, _ := namespace.Stat([]byte("/f"))
	if stat.Attributes&ramfs.AttrReadOnly == 0 {
		t.Fatal("attribute not kept")
	}
}
