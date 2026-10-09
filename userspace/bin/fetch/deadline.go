//go:build goos_userspace

package main

import (
	"errors"
	"goos/kernel/clock"
	"goos/kernel/kcall"
	"goos/userspace/runtime/sys"
	"time"
)

func (s *socket) SetDeadline(value time.Time) error {
	if value.IsZero() {
		s.deadline = 0
		return nil
	}
	// time.Now carries the overlay's kernel MONOTONIC reading, so Sub uses
	// that clock when TLS supplies a monotonic deadline.
	remaining := value.Sub(time.Now())
	now, status := sys.ClockReadNanos()
	if status != kcall.StatusOK {
		return errors.New("monotonic clock unavailable")
	}
	if remaining <= 0 {
		s.deadline = now
		if s.deadline == 0 {
			s.deadline = 1
		}
		return nil
	}
	if uint64(remaining) > ^uint64(0)-now {
		return errors.New("TLS deadline out of range")
	}
	s.deadline = now + uint64(remaining)
	return nil
}

func (s *socket) timeout() (uint64, error) {
	if s.closed {
		return 0, errors.New("TCP stream closed")
	}
	limit := uint64(35 * clock.TicksPerSecond)
	if s.deadline == 0 {
		return limit, nil
	}
	now, status := sys.ClockReadNanos()
	if status != kcall.StatusOK || now >= s.deadline {
		return 0, errors.New("TCP/TLS deadline exceeded")
	}
	tick := uint64(1000000000) / clock.TicksPerSecond
	remaining := s.deadline - now
	ticks := remaining / tick
	if remaining%tick != 0 {
		ticks++
	}
	if ticks < limit {
		limit = ticks
	}
	return limit, nil
}
