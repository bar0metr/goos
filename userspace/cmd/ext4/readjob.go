//go:build !goos_userspace || goos_goroutines

package main

import (
	"goos/kernel/ramfs"
	"goos/userspace/fsproto"
)

// readJob is one read-only request a worker executes and returns. Its name
// array owns the request name copied at admission.
type readJob struct {
	request     fsproto.Request
	correlation uint64
	reply       fsproto.Reply
	name        [ramfs.MaxName]byte
}

// adoptReadJob owns the request's variable data (its name) at admission, so
// the job does not alias the owner's decode storage after the next receive.
func adoptReadJob(request fsproto.Request, correlation uint64) readJob {
	job := readJob{request: request, correlation: correlation}
	n := copy(job.name[:], request.Name)
	job.request.Name = job.name[:n]
	return job
}
