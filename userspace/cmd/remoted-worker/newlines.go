package main

import (
	"goos/userspace/remoteservice"
	"goos/userspace/tty"
)

// ptyNewlines converts cooked output before it enters the SSH queue.
type ptyNewlines struct {
	previousCR bool
	buffer     [2 * tty.MaxPart]byte
}

func (p *ptyNewlines) Write(queue *remoteservice.Queue, data []byte, raw bool) bool {
	if len(data) == 0 {
		return true
	}
	if raw {
		if !queue.Put(data) {
			return false
		}
		p.previousCR = false
		return true
	}
	if len(data) > tty.MaxPart {
		return false
	}
	size := 0
	previousCR := p.previousCR
	for _, value := range data {
		if value == '\n' && !previousCR {
			p.buffer[size] = '\r'
			size++
		}
		p.buffer[size] = value
		size++
		previousCR = value == '\r'
	}
	if !queue.Put(p.buffer[:size]) {
		return false
	}
	p.previousCR = previousCR
	return true
}
