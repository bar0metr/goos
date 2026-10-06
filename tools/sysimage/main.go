// sysimage lays out the installed system as a directory tree, which mkfat32
// -tree copies onto a volume (ADR 0175):
//
//	system/bin/        the native commands
//	system/services/   the shell, configuration, log, and network services
//	                   and the network driver
//	system/linux/      the static Linux programs
//	system/commands    the command index
//	system/motd        the message of the day
//	system/release     the installation record
//	apps/              applications, empty
//	home/user/bin/     the user's commands, empty
//	state/             service state, empty
//
// The tree is written from the build's own outputs and nothing else, so the
// same build always yields the same tree.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"goos/tools/internal/commandset"
	linuxfixtures "goos/userspace/linux"
	"goos/userspace/release"
)

const usage = "usage: sysimage SHELL CONFIG LOG NETD VIRTIO_NET COMMAND_DIR COMMAND_INDEX OUTPUT_DIR"

func main() {
	if len(os.Args) != 9 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	output := os.Args[8]
	if err := os.RemoveAll(output); err != nil {
		fail(err)
	}
	for _, directory := range []string{"system/bin", "system/services", "system/linux", "apps", "home/user/bin", "state"} {
		if err := os.MkdirAll(filepath.Join(output, directory), 0o755); err != nil {
			fail(err)
		}
	}
	for index, name := range []string{"shell", "config", "log", "netd", "virtio-net"} {
		write(output, "system/services/"+name, read(os.Args[1+index]))
	}
	commandDirectory, index := os.Args[6], read(os.Args[7])
	names, err := commandset.Names(commandDirectory, index)
	if err != nil {
		fail(err)
	}
	for _, name := range names {
		write(output, "system/bin/"+name, read(filepath.Join(commandDirectory, name)))
	}
	write(output, "system/commands", index)
	for _, fixture := range linuxfixtures.All() {
		write(output, "system/linux/"+fixture.Name, fixture.Image)
	}
	// LINUX_EXTRA adds Linux programs the repository does not carry, such as
	// a BusyBox the interoperability test was given, each under its own
	// name. Several paths are separated as in PATH.
	for _, extra := range filepath.SplitList(os.Getenv("LINUX_EXTRA")) {
		if extra != "" {
			write(output, "system/linux/"+filepath.Base(extra), read(extra))
		}
	}
	write(output, "system/motd", []byte("Welcome to GoOS\n"))
	record := release.Current()
	write(output, "system/release", release.Encode(&record))
}

func write(root, path string, data []byte) {
	if err := os.WriteFile(filepath.Join(root, path), data, 0o644); err != nil {
		fail(err)
	}
}

func read(path string) []byte {
	data, err := os.ReadFile(path)
	if err != nil {
		fail(err)
	}
	return data
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
