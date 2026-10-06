// Package random is the kernel's random byte generator: a ChaCha20 stream
// keyed from an entropy pool, with the key replaced after every request so
// that bytes already handed out cannot be recomputed from a later state.
//
// The pool takes 64-bit samples: hardware generator output when the CPU has
// one, and the cycle counter read around work whose duration varies with
// cache, pipeline, and interrupt state. Linux seeds from the same jitter on
// a CPU without a hardware generator. The generator refuses to produce
// anything until the pool has taken SeedSamples samples.
package random

import (
	"encoding/binary"
	"goos/kernel/lock"
	"math/bits"
)

// SeedSamples is how many samples the pool takes before the first key.
// Each jitter sample is credited with well under one bit, so the count
// leaves a wide margin over the 256 bits of the key.
const SeedSamples = 4096

// reseedSamples is how many further samples fold into the next key.
const reseedSamples = 256

// Generator is safe for use from several processors; its lock is taken with
// local interrupts disabled by the caller.
type Generator struct {
	lock    lock.Spin
	pool    [16]uint32
	samples uint64
	pending uint64
	key     [8]uint32
	seeded  bool
	counter uint64
}

// Mix folds one sample into the pool. Every sixteen samples the pool is run
// through the ChaCha20 permutation, so each later key depends on every
// sample.
func (generator *Generator) Mix(sample uint64) {
	generator.lock.SetLevel(lock.LevelRandom)
	generator.lock.Lock()
	generator.mix(sample)
	generator.lock.Unlock()
}

func (generator *Generator) mix(sample uint64) {
	index := generator.samples % 8
	generator.pool[2*index] ^= uint32(sample)
	generator.pool[2*index+1] ^= uint32(sample >> 32)
	generator.samples++
	generator.pending++
	if generator.samples%8 == 0 {
		var out [16]uint32
		chachaBlock(&out, &generator.pool)
		generator.pool = out
	}
	if !generator.seeded && generator.samples >= SeedSamples || generator.seeded && generator.pending >= reseedSamples {
		generator.rekey()
	}
}

// rekey derives a new key from the old one and the pool.
func (generator *Generator) rekey() {
	var input, out [16]uint32
	for index := 0; index < 8; index++ {
		input[index] = generator.key[index] ^ generator.pool[index]
		input[8+index] = generator.pool[8+index]
	}
	chachaBlock(&out, &input)
	copy(generator.key[:], out[:8])
	// The pool keeps the other half, so the key does not reveal it.
	for index := 0; index < 8; index++ {
		generator.pool[index] ^= out[8+index]
	}
	generator.pending = 0
	generator.seeded = true
}

// Seeded reports whether the pool has taken enough samples to key the
// stream.
func (generator *Generator) Seeded() bool {
	generator.lock.SetLevel(lock.LevelRandom)
	generator.lock.Lock()
	seeded := generator.seeded
	generator.lock.Unlock()
	return seeded
}

// Read fills target from the stream and reports whether it could: nothing
// comes out before the pool is seeded.
func (generator *Generator) Read(target []byte) bool {
	generator.lock.SetLevel(lock.LevelRandom)
	generator.lock.Lock()
	if !generator.seeded {
		generator.lock.Unlock()
		return false
	}
	var block [64]byte
	for len(target) != 0 {
		generator.next(&block)
		count := copy(target, block[:])
		target = target[count:]
	}
	// Fast key erasure: the next key is stream output nobody receives.
	generator.next(&block)
	for index := range generator.key {
		generator.key[index] = binary.LittleEndian.Uint32(block[4*index:])
	}
	clear(block[:])
	generator.lock.Unlock()
	return true
}

func (generator *Generator) next(block *[64]byte) {
	var input, out [16]uint32
	input[0], input[1], input[2], input[3] = 0x61707865, 0x3320646e, 0x79622d32, 0x6b206574
	copy(input[4:12], generator.key[:])
	input[12], input[13] = uint32(generator.counter), uint32(generator.counter>>32)
	generator.counter++
	chachaBlock(&out, &input)
	for index, word := range out {
		binary.LittleEndian.PutUint32(block[4*index:], word)
	}
}

// chachaBlock is the ChaCha20 block function: twenty rounds over input, and
// input added back.
func chachaBlock(out *[16]uint32, input *[16]uint32) {
	x := *input
	for round := 0; round < 10; round++ {
		quarter(&x, 0, 4, 8, 12)
		quarter(&x, 1, 5, 9, 13)
		quarter(&x, 2, 6, 10, 14)
		quarter(&x, 3, 7, 11, 15)
		quarter(&x, 0, 5, 10, 15)
		quarter(&x, 1, 6, 11, 12)
		quarter(&x, 2, 7, 8, 13)
		quarter(&x, 3, 4, 9, 14)
	}
	for index := range out {
		out[index] = x[index] + input[index]
	}
}

func quarter(x *[16]uint32, a, b, c, d int) {
	x[a] += x[b]
	x[d] = bits.RotateLeft32(x[d]^x[a], 16)
	x[c] += x[d]
	x[b] = bits.RotateLeft32(x[b]^x[c], 12)
	x[a] += x[b]
	x[d] = bits.RotateLeft32(x[d]^x[a], 8)
	x[c] += x[d]
	x[b] = bits.RotateLeft32(x[b]^x[c], 7)
}
