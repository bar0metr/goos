//go:build goos_userspace

// Command sleep waits for a number of seconds.
package main

import (
	"goos/kernel/clock"
	"goos/userspace/commands"
	"goos/userspace/startup"
	"goos/userspace/utility"
	userwait "goos/userspace/wait"
)

var manifest = commands.Manifest{
	Usage:   "sleep SECONDS",
	Summary: "wait SECONDS (3 decimal places)",
	MinArgs: 1,
	MaxArgs: 1,
}

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }

func main() {}

func run(data startup.Data) {
	value, _ := data.Arg(1)
	duration, ok := parseSeconds(value)
	if !ok {
		utility.Fail(value, "not a number of seconds between 0 and 86400")
	}
	now := utility.Clock()
	deadline := now + duration
	for now < deadline {
		// A wait with no interests blocks until the deadline instead of
		// spinning on the clock.
		userwait.Sleep(deadline - now)
		now = utility.Clock()
	}
	utility.Mark("SLEEP_PROCESS_OK\n")
}

// parseSeconds converts seconds, with up to three decimals, into timer ticks.
// A non-zero duration never rounds down to no wait at all.
func parseSeconds(value []byte) (uint64, bool) {
	var whole, fraction uint64
	digits, seen, point := 0, false, false
	for _, character := range value {
		switch {
		case character >= '0' && character <= '9':
			seen = true
			if point {
				if digits < 3 {
					fraction = fraction*10 + uint64(character-'0')
					digits++
				}
				continue
			}
			whole = whole*10 + uint64(character-'0')
			if whole > 86400 {
				return 0, false
			}
		case character == '.' && !point:
			point = true
		default:
			return 0, false
		}
	}
	if !seen {
		return 0, false
	}
	for digits < 3 {
		fraction *= 10
		digits++
	}
	ticks := whole*clock.TicksPerSecond + fraction*clock.TicksPerSecond/1000
	if ticks == 0 && (whole != 0 || fraction != 0) {
		ticks = 1
	}
	return ticks, true
}
