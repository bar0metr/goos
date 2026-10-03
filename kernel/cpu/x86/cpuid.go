// Package x86 decodes x86 CPUID data without depending on privileged code.
// The kernel supplies a hardware Source; host tests use synthetic sources.
package x86

import "goos/internal/cpuinfo"

const (
	MaxCaches = cpuinfo.MaxCaches
	textSize  = 64
)

type Registers struct {
	EAX uint32
	EBX uint32
	ECX uint32
	EDX uint32
}

type Source interface {
	Query(leaf, subleaf uint32) Registers
}

// Text is a bounded CPUID string. Bytes after Length are always zero.
type Text struct {
	Bytes  [textSize]byte
	Length uint8
}

func (text Text) String() string { return string(text.Bytes[:text.Length]) }

type Identity struct {
	MaximumBasicLeaf    uint32
	MaximumExtendedLeaf uint32
	Vendor              Text
	Brand               Text
	HypervisorVendor    Text
	Family              uint32
	Model               uint32
	Stepping            uint32
	PhysicalAddressBits uint8
	LinearAddressBits   uint8
	BaseMHz             uint32
	MaximumMHz          uint32
	BusMHz              uint32
	BootstrapAPICID     uint32
	SignatureValid      bool
	AddressWidthsValid  bool
	FrequencyValid      bool
	HypervisorPresent   bool
	VMX                 bool
	SVM                 bool
}

type Inventory struct {
	Identity   Identity
	Topology   Topology
	Caches     [MaxCaches]Cache
	CacheCount uint8
	Hardware   cpuinfo.FeatureSet
	Usable     cpuinfo.FeatureSet
}

// Probe decodes all CPUID-backed facts. It does not infer configured CPU
// count; firmware enumeration supplies that separately.
func Probe(source Source) Inventory {
	var inventory Inventory
	ProbeInto(&inventory, source)
	return inventory
}

// ProbeInto is the stack-bounded kernel form of Probe.
//
//go:noinline
func ProbeInto(result *Inventory, source Source) {
	*result = Inventory{}
	leaf0 := source.Query(0, 0)
	extended0 := source.Query(0x80000000, 0)
	identity := Identity{
		MaximumBasicLeaf:    leaf0.EAX,
		MaximumExtendedLeaf: extended0.EAX,
		Vendor:              vendorText(leaf0),
	}

	var leaf1 Registers
	if leaf0.EAX >= 1 {
		leaf1 = source.Query(1, 0)
		identity.Family, identity.Model, identity.Stepping = decodeSignature(leaf1.EAX)
		identity.BootstrapAPICID = leaf1.EBX >> 24
		identity.SignatureValid = true
		identity.VMX = leaf1.ECX&(1<<5) != 0
		identity.HypervisorPresent = leaf1.ECX&(1<<31) != 0
		if identity.HypervisorPresent {
			identity.HypervisorVendor = hypervisorText(source.Query(0x40000000, 0))
		}
	}
	if extended0.EAX >= 0x80000001 {
		identity.SVM = source.Query(0x80000001, 0).ECX&(1<<2) != 0
	}
	if extended0.EAX >= 0x80000004 {
		identity.Brand = brandText(source)
	}
	if extended0.EAX >= 0x80000008 {
		widths := source.Query(0x80000008, 0).EAX
		identity.PhysicalAddressBits = uint8(widths)
		identity.LinearAddressBits = uint8(widths >> 8)
		identity.AddressWidthsValid = identity.PhysicalAddressBits != 0 && identity.LinearAddressBits != 0
	}
	if leaf0.EAX >= 0x16 {
		frequency := source.Query(0x16, 0)
		identity.BaseMHz = frequency.EAX
		identity.MaximumMHz = frequency.EBX
		identity.BusMHz = frequency.ECX
		identity.FrequencyValid = identity.BaseMHz != 0 || identity.MaximumMHz != 0 || identity.BusMHz != 0
	}

	topology, _ := DetectTopology(source, identity.Vendor.String(), leaf0.EAX, extended0.EAX)
	hardware, usable := DetectFeatures(source, leaf0.EAX, extended0.EAX)
	caches, cacheCount := DetectCaches(source, identity.Vendor.String(), leaf0.EAX, extended0.EAX)
	*result = Inventory{
		Identity: identity, Topology: topology, Caches: caches, CacheCount: cacheCount,
		Hardware: hardware, Usable: usable,
	}
}

func decodeSignature(signature uint32) (family, model, stepping uint32) {
	stepping = signature & 0xf
	baseModel := signature >> 4 & 0xf
	baseFamily := signature >> 8 & 0xf
	family = baseFamily
	if baseFamily == 0xf {
		family += signature >> 20 & 0xff
	}
	model = baseModel
	if baseFamily == 0x6 || baseFamily == 0xf {
		model |= signature >> 12 & 0xf0
	}
	return family, model, stepping
}

func vendorText(registers Registers) Text {
	var text Text
	put32(text.Bytes[0:4], registers.EBX)
	put32(text.Bytes[4:8], registers.EDX)
	put32(text.Bytes[8:12], registers.ECX)
	text.Length = trimmedLength(text.Bytes[:12])
	return text
}

func hypervisorText(registers Registers) Text {
	var text Text
	put32(text.Bytes[0:4], registers.EBX)
	put32(text.Bytes[4:8], registers.ECX)
	put32(text.Bytes[8:12], registers.EDX)
	text.Length = trimmedLength(text.Bytes[:12])
	return text
}

func brandText(source Source) Text {
	var text Text
	for index := uint32(0); index < 3; index++ {
		registers := source.Query(0x80000002+index, 0)
		offset := int(index) * 16
		put32(text.Bytes[offset:offset+4], registers.EAX)
		put32(text.Bytes[offset+4:offset+8], registers.EBX)
		put32(text.Bytes[offset+8:offset+12], registers.ECX)
		put32(text.Bytes[offset+12:offset+16], registers.EDX)
	}
	start, end := 0, 48
	for start < end && (text.Bytes[start] == 0 || text.Bytes[start] == ' ') {
		start++
	}
	for end > start && (text.Bytes[end-1] == 0 || text.Bytes[end-1] == ' ') {
		end--
	}
	if start != 0 {
		copy(text.Bytes[:], text.Bytes[start:end])
		clear(text.Bytes[end-start:])
	}
	text.Length = uint8(end - start)
	return text
}

func put32(target []byte, value uint32) {
	target[0] = byte(value)
	target[1] = byte(value >> 8)
	target[2] = byte(value >> 16)
	target[3] = byte(value >> 24)
}

func trimmedLength(value []byte) uint8 {
	end := len(value)
	for end > 0 && (value[end-1] == 0 || value[end-1] == ' ') {
		end--
	}
	return uint8(end)
}
