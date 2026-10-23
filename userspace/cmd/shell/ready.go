package main

import (
	"strconv"

	shellcmd "goos/userspace/shell"
)

// bootReadyToken recognizes the harness's validation-only acknowledgement.
// The caller must gate it on validation.Enabled and interactive mode. A
// positive decimal counter makes a previous command's marker unusable.
func bootReadyToken(invocation shellcmd.Invocation) ([]byte, bool) {
	if invocation.Count != 2 || string(invocation.Arguments[0]) != "__boot_ready" {
		return nil, false
	}
	token := invocation.Arguments[1]
	if len(token) == 0 || len(token) > 20 || token[0] == '0' {
		return nil, false
	}
	for _, digit := range token {
		if digit < '0' || digit > '9' {
			return nil, false
		}
	}
	counter, err := strconv.ParseUint(string(token), 10, 64)
	return token, err == nil && counter != 0
}
