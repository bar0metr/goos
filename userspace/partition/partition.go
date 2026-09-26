// Package partition finds the volumes on a block device: a filesystem on the
// whole device ("superfloppy"), an MBR partition table, or a GPT (ADR 0115).
// It only reads and describes; a filesystem is handed a block slice for the
// partition chosen, and never parses a partition table itself.
//
// Everything on the device is untrusted. Every structure is checked before
// anything is derived from it: signatures, CRCs, sizes, and that every range
// lies inside the device and apart from the others. A table that fails is
// reported, never guessed around.
package partition

// Reader reads bytes at a byte offset of the device.
type Reader interface {
	ReadAt(offset uint64, target []byte) bool
}

// Status is the outcome of a scan or a selection.
type Status uint8

const (
	StatusOK Status = iota
	// StatusIOError: the device could not be read.
	StatusIOError
	// StatusCorrupt: a table is present but contradicts itself or the
	// device, and no valid copy stands in for it.
	StatusCorrupt
	// StatusUnsupported: a structure this package does not implement, such
	// as an MBR logical partition, or a block size it does not handle.
	StatusUnsupported
	// StatusNotFound: no partition answers the selector.
	StatusNotFound
	// StatusInvalidArgument: the selector or the call is malformed.
	StatusInvalidArgument
)

// Scheme is how the device is divided.
type Scheme uint8

const (
	// SchemeNone: no table and no filesystem boot sector was recognised. The
	// whole device is the only candidate; whatever mounts it decides.
	SchemeNone Scheme = iota
	// SchemeRaw: a filesystem boot sector at LBA 0 ("superfloppy").
	SchemeRaw
	SchemeMBR
	SchemeGPT
)

func (scheme Scheme) String() string {
	switch scheme {
	case SchemeRaw:
		return "raw"
	case SchemeMBR:
		return "mbr"
	case SchemeGPT:
		return "gpt"
	}
	return "none"
}

// Kind classifies a partition by its type.
type Kind uint8

const (
	KindOther Kind = iota
	// KindData: a partition for a Windows-family filesystem: an MBR FAT32
	// type (0x0b, 0x0c and their hidden forms) or GPT basic data. The
	// filesystem it holds is for the filesystem service to recognise.
	KindData
	// KindEFI: the EFI system partition, which is FAT but belongs to the
	// firmware; it is never chosen automatically.
	KindEFI
	// KindExtended: an MBR extended partition. It is a container rather than
	// a mountable volume; its logical partitions are separate table entries.
	KindExtended
)

const (
	// MaxPartitions bounds the partitions a table may describe that are in
	// use. MBR has at most four; a GPT with more used entries is refused as
	// unsupported rather than truncated.
	MaxPartitions = 128
	// MaxName is the longest UTF-8 form of a GPT name: 36 UTF-16 units of at
	// most three bytes each.
	MaxName = 36 * 3
	// maxBlockSize is the largest logical block size handled.
	maxBlockSize = 4096
	minBlockSize = 512
)

// Partition is one used entry of a table. Start and Length are in bytes.
type Partition struct {
	// Index is the entry's number, from 1: the MBR primary slot or GPT entry;
	// MBR logical partitions use the conventional sequence starting at 5.
	Index    uint32
	Start    uint64
	Length   uint64
	Kind     Kind
	MBRType  byte
	Type     GUID
	Unique   GUID
	Name     [MaxName]byte
	NameSize uint8
}

// NameString is the partition's GPT name.
func (partition *Partition) NameString() string { return string(partition.Name[:partition.NameSize]) }

// GPTHealth is what a scan noticed about a GPT it accepted.
type GPTHealth struct {
	// UsedBackup: the primary header or its entries failed validation and
	// the backup was used. Nothing is rewritten.
	UsedBackup bool
	// BackupInvalid: the backup header or its entries failed validation,
	// or it is missing.
	BackupInvalid bool
	// BackupDiffers: both copies are valid but describe different disks or
	// entry arrays.
	BackupDiffers bool
	// BackupMisplaced: the backup header is not at the last block, as after
	// a disk image has been grown.
	BackupMisplaced bool
}

// Degraded reports whether the table was accepted with a problem.
func (health GPTHealth) Degraded() bool {
	return health.UsedBackup || health.BackupInvalid || health.BackupDiffers || health.BackupMisplaced
}

// Table is what a scan found.
type Table struct {
	Scheme     Scheme
	BlockSize  uint32
	Capacity   uint64
	Count      int
	Partitions [MaxPartitions]Partition
	DiskGUID   GUID
	GPT        GPTHealth
}

// reset empties the table field by field: it is large, and TinyGo builds a
// composite literal of it on the heap.
func (table *Table) reset(blockSize uint32, capacity uint64) {
	table.Scheme, table.BlockSize, table.Capacity, table.Count = SchemeNone, blockSize, capacity, 0
	table.DiskGUID, table.GPT = GUID{}, GPTHealth{}
	for index := range table.Partitions {
		table.Partitions[index] = Partition{}
	}
}

// add appends a partition, or reports that the table holds too many.
func (table *Table) add() (*Partition, bool) {
	if table.Count == MaxPartitions {
		return nil, false
	}
	table.Count++
	return &table.Partitions[table.Count-1], true
}

// Scanner holds the buffers a scan needs, so that a scan allocates nothing.
// One scan runs at a time.
type Scanner struct {
	block   [maxBlockSize]byte
	entries [maxBlockSize]byte
}

// Scan reads the device's partition structures. capacity is the device size
// in bytes and blockSize its logical block size, which is also the GPT's
// block size. The order follows what firmware and Linux do: a GPT behind a
// protective MBR first; then a filesystem boot sector at LBA 0, which is a
// whole-device volume; then an MBR. A protective MBR whose GPT is invalid in
// both copies is corrupt, not an MBR — unless LBA 0 is a filesystem boot
// sector, whose boot code merely looked like a protective entry.
func (scanner *Scanner) Scan(reader Reader, capacity uint64, blockSize uint32, table *Table) Status {
	table.reset(blockSize, capacity)
	size := uint64(blockSize)
	if reader == nil || size < minBlockSize || size > maxBlockSize || size&(size-1) != 0 {
		return StatusInvalidArgument
	}
	if capacity < size || capacity%size != 0 {
		return StatusInvalidArgument
	}
	first := scanner.block[:size]
	if !reader.ReadAt(0, first) {
		return StatusIOError
	}
	if first[510] != 0x55 || first[511] != 0xaa {
		return StatusOK
	}
	var mbr [4]mbrEntry
	for slot := range mbr {
		mbr[slot] = readMBREntry(first[mbrTable+slot*mbrEntrySize:])
	}
	whole := filesystemBootSector(first, capacity)
	for slot := range mbr {
		if mbr[slot].kind != mbrProtective {
			continue
		}
		status := scanner.scanGPT(reader, table)
		if status == StatusCorrupt && whole {
			// Boot code that happens to hold a protective entry does not
			// make a filesystem boot sector a GPT.
			table.reset(blockSize, capacity)
			table.Scheme = SchemeRaw
			return StatusOK
		}
		return status
	}
	if whole {
		table.Scheme = SchemeRaw
		return StatusOK
	}
	if status := scanMBR(&mbr, table); status != StatusOK || table.Scheme != SchemeMBR {
		return status
	}
	return scanner.scanExtended(reader, &mbr, table)
}

// filesystemBootSector recognises a FAT, exFAT or NTFS boot sector at LBA 0.
// The FAT test is strict — a jump instruction and a BPB whose every field is
// plausible and whose volume fits the device — because an MBR's boot code
// could otherwise pass a looser one. exFAT and NTFS are recognised by name so
// that the filesystem, not this package, reports them unsupported.
func filesystemBootSector(sector []byte, capacity uint64) bool {
	if name := string(sector[3:11]); name == "EXFAT   " || name == "NTFS    " {
		return true
	}
	if !(sector[0] == 0xeb && sector[2] == 0x90 || sector[0] == 0xe9) {
		return false
	}
	bytes := uint64(sector[11]) | uint64(sector[12])<<8
	perCluster := uint64(sector[13])
	reserved := uint64(sector[14]) | uint64(sector[15])<<8
	media := sector[21]
	total := uint64(sector[19]) | uint64(sector[20])<<8
	if total == 0 {
		total = uint64(sector[32]) | uint64(sector[33])<<8 | uint64(sector[34])<<16 | uint64(sector[35])<<24
	}
	return bytes >= 512 && bytes <= 4096 && bytes&(bytes-1) == 0 && perCluster != 0 && perCluster&(perCluster-1) == 0 &&
		reserved != 0 && sector[16] != 0 && (media == 0xf0 || media >= 0xf8) && total != 0 && total <= capacity/bytes
}

func get16(data []byte) uint16 { return uint16(data[0]) | uint16(data[1])<<8 }
func get32(data []byte) uint32 {
	return uint32(data[0]) | uint32(data[1])<<8 | uint32(data[2])<<16 | uint32(data[3])<<24
}
func get64(data []byte) uint64 { return uint64(get32(data)) | uint64(get32(data[4:]))<<32 }
func put16(data []byte, value uint16) {
	data[0], data[1] = byte(value), byte(value>>8)
}
func put32(data []byte, value uint32) {
	data[0], data[1], data[2], data[3] = byte(value), byte(value>>8), byte(value>>16), byte(value>>24)
}
func put64(data []byte, value uint64) {
	put32(data, uint32(value))
	put32(data[4:], uint32(value>>32))
}
