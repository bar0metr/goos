//go:build goos_userspace

package main

import (
	"goos/kernel/vfs"
	vfsservice "goos/userspace/vfs"
)

// listingStart holds the counters at the end of the previous listing.
var (
	listingStart    vfs.AuthorityCounters
	listingReadDirs uint64
	listingBackend  vfsservice.BackendBatchCounters
)

// reportListing writes what one directory listing cost when its scan ends, so
// a DIAG build shows the backend work behind `time ls`. The line goes out
// before the final reply, ahead of the client's next prompt.
func reportListing() {
	counters, readDirs := service.ListingCounters()
	backend := service.BackendBatchCounters()
	write("VFS_LISTING requests=")
	writeDecimal(counters.ReadDirRequests - listingStart.ReadDirRequests)
	write(" scope_checks=")
	writeDecimal(counters.ScopeChecks - listingStart.ScopeChecks)
	write(" scope_contains=")
	writeDecimal(counters.ScopeContains - listingStart.ScopeContains)
	write(" scope_openname=")
	writeDecimal(counters.ScopeOpenName - listingStart.ScopeOpenName)
	write(" scope_backend_readdir=")
	writeDecimal(counters.ScopeBackendReadDir - listingStart.ScopeBackendReadDir)
	write(" fat32_readdir=")
	writeDecimal(readDirs - listingReadDirs)
	write(" batch_requests=")
	writeDecimal(counters.BatchRequests - listingStart.BatchRequests)
	write(" batch_entries=")
	writeDecimal(counters.BatchEntries - listingStart.BatchEntries)
	write(" fat32_batch_requests=")
	writeDecimal(backend.Requests - listingBackend.Requests)
	write(" fat32_batch_entries=")
	writeDecimal(backend.Entries - listingBackend.Entries)
	write("\n")
	listingStart, listingReadDirs, listingBackend = counters, readDirs, backend
}
