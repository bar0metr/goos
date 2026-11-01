//go:build goos_userspace

package main

import (
	"goos/kernel/vfs"
)

// listingStart holds the counters at the end of the previous listing.
var (
	listingStart    vfs.AuthorityCounters
	listingReadDirs uint64
)

// reportListing writes what one directory listing cost when its scan ends, so
// a DIAG build shows the backend work behind `time ls`. The line goes out
// before the final reply, ahead of the client's next prompt.
func reportListing(status vfs.Status) {
	if status != vfs.StatusNotFound {
		return
	}
	counters, readDirs := service.ListingCounters()
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
	write("\n")
	listingStart, listingReadDirs = counters, readDirs
}
