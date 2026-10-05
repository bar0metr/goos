//go:build uefi

package main

// The kernel's random bytes (ADR 0185): AT_RANDOM of every Linux image and
// getrandom come from one generator, keyed from hardware output when the CPU
// has a generator and from timing jitter always.

import (
	"goos/kernel/arch/amd64"
	"goos/kernel/random"
)

var (
	randomGenerator random.Generator
	// jitterScratch is memory the seeding loop walks, so its duration
	// depends on cache state as well as on interrupts and the pipeline.
	jitterScratch [4096]byte
)

// seedRandom fills the pool before the first process exists. It takes the
// hardware generator's output when there is one, and the cycle counter
// around work of varying length SeedSamples times in any case.
func seedRandom() {
	var hardware [8]byte
	for draw := 0; draw < 32 && amd64.FillRandom(hardware[:]); draw++ {
		randomGenerator.Mix(uint64(hardware[0]) | uint64(hardware[1])<<8 | uint64(hardware[2])<<16 | uint64(hardware[3])<<24 |
			uint64(hardware[4])<<32 | uint64(hardware[5])<<40 | uint64(hardware[6])<<48 | uint64(hardware[7])<<56)
	}
	clear(hardware[:])
	delta, position := uint64(0), uint64(0)
	for sample := 0; sample < random.SeedSamples || !randomGenerator.Seeded(); sample++ {
		start := amd64.ReadTSC()
		for step := uint64(0); step < 16+delta%32; step++ {
			position = (position*33 + delta + step) % uint64(len(jitterScratch))
			jitterScratch[position] += byte(start)
		}
		end := amd64.ReadTSC()
		delta = end - start
		randomGenerator.Mix(delta ^ end<<24 ^ uint64(jitterScratch[position]))
	}
}

// stirRandom folds the cycle counter into the pool. The kernel calls it at
// points whose timing depends on what processes and devices do.
func stirRandom() { randomGenerator.Mix(amd64.ReadTSC()) }

// randomBytes fills target with generator output.
func randomBytes(target []byte) bool { return randomGenerator.Read(target) }
