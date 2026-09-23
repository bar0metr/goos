//go:build uefi && !goos_validation

package main

import "goos/kernel/arch/amd64"

// runFilesystemSelfTests is a no-op in normal builds, which link no filesystem
// code (ADR 0084).
func runFilesystemSelfTests(console amd64.COM1) {}
