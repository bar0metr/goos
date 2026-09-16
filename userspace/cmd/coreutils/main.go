//go:build goos_userspace

package main

import "goos/userspace/utility"

//export userspaceMain
func userspaceMain() { utility.Run() }
func main()          {}
