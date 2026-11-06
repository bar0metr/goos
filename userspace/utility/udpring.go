//go:build goos_userspace

package utility

import (
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/userspace/runtime/sys"
	"goos/userspace/startup"
	"goos/userspace/udpapi"
)

// OpenUDPRing maps the GUDP 3 session a GrantUDPRing command receives
// (ADR 0216) and returns its channel and its two whole pools, each with its
// control page. The command drives the rings itself.
func OpenUDPRing(data startup.Data) (channel handle.ID, tx, rx []byte, ok bool) {
	channel, found := data.Handle("udp")
	txHandle, txFound := data.Handle("udp-tx")
	rxHandle, rxFound := data.Handle("udp-rx")
	if !found || !txFound || !rxFound {
		return 0, nil, nil, false
	}
	txBase, txSize, txStatus := sys.MemoryMapAnywhere(txHandle, true)
	rxBase, rxSize, rxStatus := sys.MemoryMapAnywhere(rxHandle, false)
	const txPool, rxPool = udpapi.TXPoolPages3 * 4096, udpapi.RXPoolPages3 * 4096
	if txStatus != kcall.StatusOK || rxStatus != kcall.StatusOK || txSize < txPool || rxSize < rxPool {
		return 0, nil, nil, false
	}
	return channel, sys.Window(txBase, txPool), sys.Window(rxBase, rxPool), true
}
