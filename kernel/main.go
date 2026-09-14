//go:build uefi

package main

import "goos/kernel/arch/amd64"

func main() {
	console := amd64.NewCOM1()
	console.Init()
	console.WriteString("GoOS 0.1-dev\n")
	console.WriteString("TinyGo kernel\n\n")
	console.WriteString("CPU: x86_64\n")
	console.WriteString("Boot: UEFI\n")
	console.WriteString("Security: capabilities (foundation)\n")
	console.WriteString("Kernel initialized\n")
	console.WriteString("BOOT_OK\n")

	for {
		amd64.Halt()
	}
}
