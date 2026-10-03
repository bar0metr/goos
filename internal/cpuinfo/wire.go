package cpuinfo

import "encoding/binary"

const (
	Version       = 1
	SummarySize   = 320
	CPUEntrySize  = 32
	CacheInfoSize = 48
)

const (
	ValidIdentity uint32 = 1 << iota
	ValidTopology
	ValidAddressWidths
	ValidFrequency
	ValidHypervisor
)

const (
	ArchitectureX86_64 = 1
	ByteOrderLittle    = 1
	Mode32             = 1 << 0
	Mode64             = 1 << 1
)

const (
	CPUEntryPresent uint32 = 1 << iota
	CPUEntryOnline
	CPUEntryBootstrap
	CPUEntryTopologyValid
	CPUEntryX2APIC
	CPUEntryACPIUIDValid
)

type Summary struct {
	Validity              uint32
	PresentLogicalCPUs    uint32
	OnlineCPUs            uint32
	Family                uint32
	Model                 uint32
	Stepping              uint32
	ThreadsPerCore        uint16
	CoresPerSocket        uint16
	Sockets               uint16
	LogicalCPUsPerPackage uint16
	PhysicalAddressBits   uint8
	LinearAddressBits     uint8
	OperatingModes        uint8
	ByteOrder             uint8
	BaseMHz               uint32
	MaximumMHz            uint32
	BusMHz                uint32
	BootstrapAPICID       uint32
	MaximumBasicLeaf      uint32
	MaximumExtendedLeaf   uint32
	Vendor                [16]byte
	Brand                 [64]byte
	HypervisorVendor      [16]byte
	HardwareFeatures      FeatureSet
	UsableFeatures        FeatureSet
}

type CPUEntry struct {
	LogicalID uint32
	APICID    uint32
	ACPIUID   uint32
	Flags     uint32
	PackageID uint32
	CoreID    uint32
	ThreadID  uint32
}

type CacheInfo struct {
	Validity     uint32
	Level        uint8
	Type         CacheType
	LineSize     uint32
	Partitions   uint32
	Ways         uint32
	Sets         uint32
	SharingCount uint32
	Size         uint64
}

func (summary Summary) Encode(target []byte) bool {
	if len(target) != SummarySize {
		return false
	}
	clear(target)
	put16(target[0:2], Version)
	put16(target[2:4], SummarySize)
	put32(target[4:8], summary.Validity)
	put32(target[8:12], summary.PresentLogicalCPUs)
	put32(target[12:16], summary.OnlineCPUs)
	put32(target[16:20], summary.Family)
	put32(target[20:24], summary.Model)
	put32(target[24:28], summary.Stepping)
	put16(target[28:30], summary.ThreadsPerCore)
	put16(target[30:32], summary.CoresPerSocket)
	put16(target[32:34], summary.Sockets)
	put16(target[34:36], summary.LogicalCPUsPerPackage)
	target[36], target[37] = summary.PhysicalAddressBits, summary.LinearAddressBits
	target[38], target[39] = summary.OperatingModes, summary.ByteOrder
	put32(target[40:44], summary.BaseMHz)
	put32(target[44:48], summary.MaximumMHz)
	put32(target[48:52], summary.BusMHz)
	put32(target[52:56], summary.BootstrapAPICID)
	put32(target[56:60], summary.MaximumBasicLeaf)
	put32(target[60:64], summary.MaximumExtendedLeaf)
	copy(target[64:80], summary.Vendor[:])
	copy(target[80:144], summary.Brand[:])
	copy(target[144:160], summary.HypervisorVendor[:])
	encodeFeatures(target[160:224], summary.HardwareFeatures)
	encodeFeatures(target[224:288], summary.UsableFeatures)
	return true
}

func DecodeSummary(source []byte) (Summary, bool) {
	if len(source) != SummarySize || read16(source[0:2]) != Version || read16(source[2:4]) != SummarySize {
		return Summary{}, false
	}
	var summary Summary
	summary.Validity = read32(source[4:8])
	summary.PresentLogicalCPUs = read32(source[8:12])
	summary.OnlineCPUs = read32(source[12:16])
	summary.Family = read32(source[16:20])
	summary.Model = read32(source[20:24])
	summary.Stepping = read32(source[24:28])
	summary.ThreadsPerCore = read16(source[28:30])
	summary.CoresPerSocket = read16(source[30:32])
	summary.Sockets = read16(source[32:34])
	summary.LogicalCPUsPerPackage = read16(source[34:36])
	summary.PhysicalAddressBits, summary.LinearAddressBits = source[36], source[37]
	summary.OperatingModes, summary.ByteOrder = source[38], source[39]
	summary.BaseMHz = read32(source[40:44])
	summary.MaximumMHz = read32(source[44:48])
	summary.BusMHz = read32(source[48:52])
	summary.BootstrapAPICID = read32(source[52:56])
	summary.MaximumBasicLeaf = read32(source[56:60])
	summary.MaximumExtendedLeaf = read32(source[60:64])
	copy(summary.Vendor[:], source[64:80])
	copy(summary.Brand[:], source[80:144])
	copy(summary.HypervisorVendor[:], source[144:160])
	summary.HardwareFeatures = decodeFeatures(source[160:224])
	summary.UsableFeatures = decodeFeatures(source[224:288])
	return summary, true
}

func (entry CPUEntry) Encode(target []byte) bool {
	if len(target) != CPUEntrySize {
		return false
	}
	clear(target)
	put32(target[0:4], entry.LogicalID)
	put32(target[4:8], entry.APICID)
	put32(target[8:12], entry.ACPIUID)
	put32(target[12:16], entry.Flags)
	put32(target[16:20], entry.PackageID)
	put32(target[20:24], entry.CoreID)
	put32(target[24:28], entry.ThreadID)
	return true
}

func DecodeCPUEntry(source []byte) (CPUEntry, bool) {
	if len(source) != CPUEntrySize {
		return CPUEntry{}, false
	}
	return CPUEntry{
		LogicalID: read32(source[0:4]), APICID: read32(source[4:8]),
		ACPIUID: read32(source[8:12]), Flags: read32(source[12:16]),
		PackageID: read32(source[16:20]), CoreID: read32(source[20:24]),
		ThreadID: read32(source[24:28]),
	}, true
}

func (cache CacheInfo) Encode(target []byte) bool {
	if len(target) != CacheInfoSize {
		return false
	}
	clear(target)
	put16(target[0:2], Version)
	put16(target[2:4], CacheInfoSize)
	put32(target[4:8], cache.Validity)
	target[8], target[9] = cache.Level, byte(cache.Type)
	put32(target[12:16], cache.LineSize)
	put32(target[16:20], cache.Partitions)
	put32(target[20:24], cache.Ways)
	put32(target[24:28], cache.Sets)
	put32(target[28:32], cache.SharingCount)
	put64(target[32:40], cache.Size)
	return true
}

func DecodeCacheInfo(source []byte) (CacheInfo, bool) {
	if len(source) != CacheInfoSize || read16(source[0:2]) != Version || read16(source[2:4]) != CacheInfoSize {
		return CacheInfo{}, false
	}
	return CacheInfo{
		Validity: read32(source[4:8]), Level: source[8], Type: CacheType(source[9]),
		LineSize: read32(source[12:16]), Partitions: read32(source[16:20]),
		Ways: read32(source[20:24]), Sets: read32(source[24:28]),
		SharingCount: read32(source[28:32]), Size: read64(source[32:40]),
	}, true
}

func encodeFeatures(target []byte, features FeatureSet) {
	for index, value := range features {
		put64(target[index*8:index*8+8], value)
	}
}

func decodeFeatures(source []byte) FeatureSet {
	var features FeatureSet
	for index := range features {
		features[index] = read64(source[index*8 : index*8+8])
	}
	return features
}

func put16(target []byte, value uint16) { binary.LittleEndian.PutUint16(target, value) }
func put32(target []byte, value uint32) { binary.LittleEndian.PutUint32(target, value) }
func put64(target []byte, value uint64) { binary.LittleEndian.PutUint64(target, value) }
func read16(source []byte) uint16       { return binary.LittleEndian.Uint16(source) }
func read32(source []byte) uint32       { return binary.LittleEndian.Uint32(source) }
func read64(source []byte) uint64       { return binary.LittleEndian.Uint64(source) }
