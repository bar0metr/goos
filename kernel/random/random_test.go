package random

import (
	"bytes"
	"testing"
)

// RFC 8439 section 2.3.2.
func TestChaChaBlockVector(t *testing.T) {
	input := [16]uint32{
		0x61707865, 0x3320646e, 0x79622d32, 0x6b206574,
		0x03020100, 0x07060504, 0x0b0a0908, 0x0f0e0d0c,
		0x13121110, 0x17161514, 0x1b1a1918, 0x1f1e1d1c,
		0x00000001, 0x09000000, 0x4a000000, 0x00000000,
	}
	want := [16]uint32{
		0xe4e7f110, 0x15593bd1, 0x1fdd0f50, 0xc47120a3,
		0xc7f4d1c7, 0x0368c033, 0x9aaa2204, 0x4e6cd4c3,
		0x466482d2, 0x09aa9f07, 0x05d7c214, 0xa2028bd9,
		0xd19c12b5, 0xb94e16de, 0xe883d0cb, 0x4e3c50a2,
	}
	var out [16]uint32
	chachaBlock(&out, &input)
	if out != want {
		t.Fatalf("block = %08x", out)
	}
}

func TestNothingBeforeSeed(t *testing.T) {
	var generator Generator
	buffer := make([]byte, 16)
	for sample := 0; sample < SeedSamples-1; sample++ {
		generator.Mix(uint64(sample))
	}
	if generator.Seeded() || generator.Read(buffer) {
		t.Fatal("bytes before the pool was seeded")
	}
	generator.Mix(1)
	if !generator.Seeded() || !generator.Read(buffer) {
		t.Fatal("no bytes after seeding")
	}
}

func TestOutputsDifferAndKeyMoves(t *testing.T) {
	var generator Generator
	for sample := 0; sample < SeedSamples; sample++ {
		generator.Mix(uint64(sample) * 0x9e3779b97f4a7c15)
	}
	first, second := make([]byte, 100), make([]byte, 100)
	key := generator.key
	generator.Read(first)
	if generator.key == key {
		t.Fatal("key not replaced after a read")
	}
	generator.Read(second)
	if bytes.Equal(first, second) || bytes.Equal(first[:32], make([]byte, 32)) {
		t.Fatal("repeated or empty output")
	}
}

func TestDifferentSamplesGiveDifferentStreams(t *testing.T) {
	var a, b Generator
	for sample := 0; sample < SeedSamples; sample++ {
		a.Mix(uint64(sample))
		b.Mix(uint64(sample) + 1)
	}
	first, second := make([]byte, 32), make([]byte, 32)
	a.Read(first)
	b.Read(second)
	if bytes.Equal(first, second) {
		t.Fatal("different pools gave one stream")
	}
}
