//go:build !uefi

package main

// main exists only so host-side tooling can type-check the kernel package.
// The bootable entry point is selected by TinyGo's uefi build tag.
func main() {}
