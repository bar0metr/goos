package tty

// OutputSource is the reading end of a terminal's output stream.
type OutputSource interface {
	// ReadOutput fills buffer and returns how many bytes it holds. empty
	// reports a stream with nothing to read now, and closed one that has
	// ended or failed.
	ReadOutput(buffer []byte) (count int, empty bool, closed bool)
}

// Relay passes output from source to the terminal until source has nothing
// more or at least limit bytes have passed. It reports whether any byte
// passed and whether the stream ended.
//
// The service relays a bounded amount on each pass so a program that writes
// without pause cannot hold off console input. Before it acts on a session
// request it relays up to the stream's capacity: the client wrote its output
// before sending the request, so all of it is then on the terminal and the
// request's prompt follows it.
func (engine *Engine) Relay(source OutputSource, buffer []byte, limit int) (relayed bool, closed bool) {
	for passed := 0; passed < limit; {
		count, empty, ended := source.ReadOutput(buffer)
		if empty && !ended {
			return relayed, false
		}
		// A read that succeeds with no bytes, or more than were asked for,
		// is a broken stream.
		if ended || count == 0 || count > len(buffer) {
			return relayed, true
		}
		engine.Output(buffer[:count])
		relayed = true
		passed += count
	}
	return relayed, false
}
