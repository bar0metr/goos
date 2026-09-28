//go:build uefi && !goos_validation

package main

import kernelconsole "goos/kernel/console"

// runFilesystemSelfTests is a no-op in normal builds, which link no filesystem
// code (ADR 0084).
func runFilesystemSelfTests(console kernelconsole.Writer) {}
