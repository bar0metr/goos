package tcpupstream

import "goos/userspace/tcpapi"

// The resolver's local stub (userspace/dnsstub) serves clients over the same
// TCP session. It takes TX slots from this client's tracker, so the two
// never lend one slot twice, and holds at most TXSlots-MaxStreams of them:
// each fallback has at most one Write out, so the fallbacks always find one.

// TXSlots is the TX slot tracker of the session, valid from Bind on.
func (c *Client) TXSlots() *tcpapi.Slots { return &c.txSlots }
