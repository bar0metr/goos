//go:build goos_userspace

// Command fetch transfers one HTTP/1.1 response through resolver, TCP and VFS grants.
package main

import (
	"goos/userspace/commands"
	"goos/userspace/fetch"
	"goos/userspace/fetchtransport"
	"goos/userspace/runtime/arena"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage: "fetch [OPTIONS] URL", Summary: "HTTP transfer",
	Options: "-I -i -v -H HEADER -d DATA -o FILE -O -L",
	MinArgs: 1, MaxArgs: 99,
	Grants: commands.GrantResolve | commands.GrantTCP | commands.GrantVFS,
}
var options fetch.Options
var arguments [commands.MaxArgs][]byte

//export userspaceMain
func userspaceMain() { arena.InitPackages(); utility.Run(manifest, run) }
func main()          {}

func run(data startup.Data) {
	for i := 0; i < data.ArgCount(); i++ {
		arguments[i], _ = data.Arg(i + 1)
	}
	if fetch.Parse(arguments[:data.ArgCount()], &options) != nil {
		utility.Usage()
	}
	transport, err := fetchtransport.New(data)
	if err != nil {
		utility.FailStatus(1, nil, err.Error())
	}
	if err := fetch.Run(&options, transport); err != nil {
		utility.FailStatus(1, nil, err.Error())
	}
}
