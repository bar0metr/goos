package x86

type Topology struct {
	ThreadsPerCore        uint16
	LogicalCPUsPerPackage uint16
	CoresPerPackage       uint16
	SMTShift              uint8
	PackageShift          uint8
	BootstrapX2APICID     uint32
	Valid                 bool
}

type TopologyID struct {
	Package uint32
	Core    uint32
	Thread  uint32
}

func DetectTopology(source Source, vendor string, maximumBasicLeaf, maximumExtendedLeaf uint32) (Topology, bool) {
	if maximumBasicLeaf >= 0x1f {
		if topology, ok := topologyLeaf(source, 0x1f); ok {
			return topology, true
		}
	}
	if maximumBasicLeaf >= 0x0b {
		if topology, ok := topologyLeaf(source, 0x0b); ok {
			return topology, true
		}
	}
	return legacyTopology(source, vendor, maximumBasicLeaf, maximumExtendedLeaf)
}

func legacyTopology(source Source, vendor string, maximumBasicLeaf, maximumExtendedLeaf uint32) (Topology, bool) {
	if maximumBasicLeaf < 1 {
		return Topology{}, false
	}
	leaf1 := source.Query(1, 0)
	logical := (leaf1.EBX >> 16) & 0xff
	if logical <= 1 || !powerOfTwo(logical) {
		return Topology{}, false
	}
	var cores uint32
	switch vendor {
	case "GenuineIntel":
		if maximumBasicLeaf < 4 {
			return Topology{}, false
		}
		leaf4 := source.Query(4, 0)
		if leaf4.EAX&0x1f == 0 {
			return Topology{}, false
		}
		cores = (leaf4.EAX>>26)&0x3f + 1
	case "AuthenticAMD":
		if maximumExtendedLeaf < 0x80000008 {
			return Topology{}, false
		}
		leaf8 := source.Query(0x80000008, 0)
		cores = leaf8.ECX&0xff + 1
		coreIDBits := leaf8.ECX >> 12 & 0xf
		if coreIDBits != 0 && uint32(1)<<coreIDBits != cores {
			return Topology{}, false
		}
	default:
		return Topology{}, false
	}
	if cores == 0 || !powerOfTwo(cores) || logical%cores != 0 {
		return Topology{}, false
	}
	threads := logical / cores
	if !powerOfTwo(threads) {
		return Topology{}, false
	}
	topology := Topology{
		ThreadsPerCore:        uint16(threads),
		LogicalCPUsPerPackage: uint16(logical),
		CoresPerPackage:       uint16(cores),
		SMTShift:              log2PowerOfTwo(threads),
		PackageShift:          log2PowerOfTwo(logical),
		BootstrapX2APICID:     leaf1.EBX >> 24,
		Valid:                 true,
	}
	return topology, true
}

func powerOfTwo(value uint32) bool { return value != 0 && value&(value-1) == 0 }

func log2PowerOfTwo(value uint32) uint8 {
	var shift uint8
	for value > 1 {
		value >>= 1
		shift++
	}
	return shift
}

func topologyLeaf(source Source, leaf uint32) (Topology, bool) {
	var topology Topology
	var previousShift uint32
	var smtSeen, coreSeen bool
	for subleaf := uint32(0); subleaf < 32; subleaf++ {
		registers := source.Query(leaf, subleaf)
		levelType := uint8(registers.ECX >> 8)
		count := registers.EBX & 0xffff
		if levelType == 0 || count == 0 {
			break
		}
		shift := registers.EAX & 0x1f
		if shift < previousShift || shift >= 32 || subleaf != 0 && shift == previousShift {
			return Topology{}, false
		}
		if subleaf != 0 && registers.EDX != topology.BootstrapX2APICID {
			return Topology{}, false
		}
		topology.BootstrapX2APICID = registers.EDX
		previousShift = shift
		switch levelType {
		case 1:
			if smtSeen || count > 0xffff || shift == 0 && count != 1 {
				return Topology{}, false
			}
			topology.ThreadsPerCore = uint16(count)
			topology.SMTShift = uint8(shift)
			smtSeen = true
		case 2:
			if coreSeen || !smtSeen || shift <= uint32(topology.SMTShift) || count > 0xffff {
				return Topology{}, false
			}
			topology.LogicalCPUsPerPackage = uint16(count)
			topology.PackageShift = uint8(shift)
			coreSeen = true
		}
	}
	if !smtSeen || !coreSeen || topology.ThreadsPerCore == 0 || topology.LogicalCPUsPerPackage == 0 ||
		topology.LogicalCPUsPerPackage%topology.ThreadsPerCore != 0 ||
		uint32(topology.ThreadsPerCore) > uint32(1)<<topology.SMTShift ||
		uint32(topology.LogicalCPUsPerPackage) > uint32(1)<<topology.PackageShift {
		return Topology{}, false
	}
	topology.CoresPerPackage = topology.LogicalCPUsPerPackage / topology.ThreadsPerCore
	topology.Valid = true
	return topology, true
}

func (topology Topology) ID(apicID uint32) (TopologyID, bool) {
	if !topology.Valid || topology.PackageShift >= 32 || topology.SMTShift >= topology.PackageShift {
		return TopologyID{}, false
	}
	threadMask := uint32(1)<<topology.SMTShift - 1
	coreMask := uint32(1)<<(topology.PackageShift-topology.SMTShift) - 1
	return TopologyID{
		Package: apicID >> topology.PackageShift,
		Core:    apicID >> topology.SMTShift & coreMask,
		Thread:  apicID & threadMask,
	}, true
}
