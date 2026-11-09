package ext4

// Directory index hashes. The algorithms are the on-disk contract of the
// hash tree: a name must hash to the same value Linux computed when it
// placed the entry. Versions 3-5 are the unsigned-char variants a volume
// selects through the superblock flags.

const (
	hashLegacy = iota
	hashHalfMD4
	hashTEA
	hashLegacyUnsigned
	hashHalfMD4Unsigned
	hashTEAUnsigned
	hashSipHash

	htreeEOF32 = 0x7FFFFFFF
)

func rotl32(value uint32, shift uint) uint32 { return value<<shift | value>>(32-shift) }

func teaTransform(buffer *[4]uint32, input *[8]uint32) {
	const delta = 0x9E3779B9
	var sum uint32
	b0, b1 := buffer[0], buffer[1]
	a, b, c, d := input[0], input[1], input[2], input[3]
	for round := 0; round < 16; round++ {
		sum += delta
		b0 += ((b1 << 4) + a) ^ (b1 + sum) ^ ((b1 >> 5) + b)
		b1 += ((b0 << 4) + c) ^ (b0 + sum) ^ ((b0 >> 5) + d)
	}
	buffer[0] += b0
	buffer[1] += b1
}

func halfMD4Transform(buffer *[4]uint32, input *[8]uint32) {
	const (
		k2 = 0x5A827999
		k3 = 0x6ED9EBA1
	)
	f := func(x, y, z uint32) uint32 { return z ^ (x & (y ^ z)) }
	g := func(x, y, z uint32) uint32 { return (x & y) + ((x ^ y) & z) }
	h := func(x, y, z uint32) uint32 { return x ^ y ^ z }
	a, b, c, d := buffer[0], buffer[1], buffer[2], buffer[3]
	in := input

	a = rotl32(a+f(b, c, d)+in[0], 3)
	d = rotl32(d+f(a, b, c)+in[1], 7)
	c = rotl32(c+f(d, a, b)+in[2], 11)
	b = rotl32(b+f(c, d, a)+in[3], 19)
	a = rotl32(a+f(b, c, d)+in[4], 3)
	d = rotl32(d+f(a, b, c)+in[5], 7)
	c = rotl32(c+f(d, a, b)+in[6], 11)
	b = rotl32(b+f(c, d, a)+in[7], 19)

	a = rotl32(a+g(b, c, d)+in[1]+k2, 3)
	d = rotl32(d+g(a, b, c)+in[3]+k2, 5)
	c = rotl32(c+g(d, a, b)+in[5]+k2, 9)
	b = rotl32(b+g(c, d, a)+in[7]+k2, 13)
	a = rotl32(a+g(b, c, d)+in[0]+k2, 3)
	d = rotl32(d+g(a, b, c)+in[2]+k2, 5)
	c = rotl32(c+g(d, a, b)+in[4]+k2, 9)
	b = rotl32(b+g(c, d, a)+in[6]+k2, 13)

	a = rotl32(a+h(b, c, d)+in[3]+k3, 3)
	d = rotl32(d+h(a, b, c)+in[7]+k3, 9)
	c = rotl32(c+h(d, a, b)+in[2]+k3, 11)
	b = rotl32(b+h(c, d, a)+in[6]+k3, 15)
	a = rotl32(a+h(b, c, d)+in[1]+k3, 3)
	d = rotl32(d+h(a, b, c)+in[5]+k3, 9)
	c = rotl32(c+h(d, a, b)+in[0]+k3, 11)
	b = rotl32(b+h(c, d, a)+in[4]+k3, 15)

	buffer[0] += a
	buffer[1] += b
	buffer[2] += c
	buffer[3] += d
}

func legacyHash(name []byte, signed bool) uint32 {
	hash0, hash1 := uint32(0x12A3FE2D), uint32(0x37ABE8F9)
	for _, value := range name {
		char := int32(value)
		if signed {
			char = int32(int8(value))
		}
		hash := hash1 + (hash0 ^ uint32(char*7152373))
		if hash&0x80000000 != 0 {
			hash -= 0x7FFFFFFF
		}
		hash1, hash0 = hash0, hash
	}
	return hash0 << 1
}

// nameToWords packs up to 4*count bytes of name into words the way the
// hash functions expect, padding with a value derived from the length.
func nameToWords(name []byte, words *[8]uint32, count int, signed bool) {
	length := uint32(len(name))
	pad := length | length<<8
	pad |= pad << 16
	value := pad
	if len(name) > count*4 {
		name = name[:count*4]
	}
	out := 0
	for index, raw := range name {
		char := uint32(raw)
		if signed {
			char = uint32(int32(int8(raw)))
		}
		value = char + value<<8
		if index%4 == 3 {
			words[out] = value
			out++
			value = pad
		}
	}
	if out < count {
		words[out] = value
		out++
	}
	for ; out < count; out++ {
		words[out] = pad
	}
}

// dirHash computes the major hash of name under version with the volume
// seed. ok is false for versions this implementation does not compute.
func dirHash(name []byte, version uint8, seed *[4]uint32) (uint32, bool) {
	buffer := [4]uint32{0x67452301, 0xEFCDAB89, 0x98BADCFE, 0x10325476}
	if seed[0]|seed[1]|seed[2]|seed[3] != 0 {
		buffer = *seed
	}
	var hash uint32
	var words [8]uint32
	switch version {
	case hashLegacy, hashLegacyUnsigned:
		hash = legacyHash(name, version == hashLegacy)
	case hashHalfMD4, hashHalfMD4Unsigned:
		signed := version == hashHalfMD4
		for rest := name; len(rest) > 0; rest = rest[32:] {
			nameToWords(rest, &words, 8, signed)
			halfMD4Transform(&buffer, &words)
			if len(rest) <= 32 {
				break
			}
		}
		hash = buffer[1]
	case hashTEA, hashTEAUnsigned:
		signed := version == hashTEA
		for rest := name; len(rest) > 0; rest = rest[16:] {
			nameToWords(rest, &words, 4, signed)
			teaTransform(&buffer, &words)
			if len(rest) <= 16 {
				break
			}
		}
		hash = buffer[0]
	default:
		return 0, false
	}
	hash &^= 1
	if hash == htreeEOF32<<1 {
		hash = (htreeEOF32 - 1) << 1
	}
	return hash, true
}
