package upstream

import "goos/userspace/udpapi"

// The resolver's local stub (userspace/dnsstub) answers clients over the
// same UDP session. It takes TX slots from this client's tracker, so the two
// never lend one slot twice, and leaves one for every query still opening.

// TXSlots is the TX slot tracker of the session, valid from Bind on.
func (c *Client) TXSlots() *udpapi.Slots { return &c.txSlots }

// Opening counts the queries whose endpoint has not arrived yet. Each takes
// a TX slot when it does.
func (c *Client) Opening() int {
	count := 0
	for index := range c.queries {
		if c.queries[index].phase == opening {
			count++
		}
	}
	return count
}
