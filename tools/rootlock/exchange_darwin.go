package main

import "golang.org/x/sys/unix"

func exchange(a, b string) error {
	return unix.RenamexNp(a, b, unix.RENAME_SWAP)
}
