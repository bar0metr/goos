package main

import (
	"testing"

	"goos/userspace/fsproto"
)

// TestAdoptReadJobOwnsName verifies the admission-time name copy: the job's
// request must resolve the original name even after the owner's decode storage
// is overwritten by a later request.
func TestAdoptReadJobOwnsName(t *testing.T) {
	storage := []byte("/dir/alpha")
	request := fsproto.Request{Operation: fsproto.OperationOpen, Name: storage}
	job := adoptReadJob(request, 1)
	// The owner receives and decodes another request over the same storage
	// before the worker consumes the first job.
	copy(storage, []byte("/dir/beta"))
	if string(job.request.Name) != "/dir/alpha" {
		t.Fatalf("job name %q, want %q", job.request.Name, "/dir/alpha")
	}
}
