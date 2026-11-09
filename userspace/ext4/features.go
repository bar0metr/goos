package ext4

// FeatureClass is the superblock word a feature bit lives in.
type FeatureClass uint8

const (
	Compat FeatureClass = iota + 1
	Incompat
	ROCompat
)

func (class FeatureClass) String() string {
	switch class {
	case Compat:
		return "compat"
	case Incompat:
		return "incompat"
	case ROCompat:
		return "ro_compat"
	}
	return "unknown"
}

// Support is what this implementation does with a volume that has a feature.
type Support uint8

const (
	// Reject: the volume is refused.
	Reject Support = iota + 1
	// ReadOnly: the volume can be mounted read-only, never read-write.
	ReadOnly
	// ReadWrite: the feature is part of the writable profile.
	ReadWrite
)

func (support Support) String() string {
	switch support {
	case Reject:
		return "reject"
	case ReadOnly:
		return "read-only"
	case ReadWrite:
		return "read-write"
	}
	return "unknown"
}

// Feature is one row of the feature matrix. docs/development/ext4-feature-
// matrix.md renders the same table and a unit test keeps the two equal.
type Feature struct {
	Name    string
	Class   FeatureClass
	Mask    uint32
	Support Support
	// Note states the constraint behind the decision.
	Note string
}

const (
	compatDirPrealloc   = 0x1
	compatImagicInodes  = 0x2
	compatHasJournal    = 0x4
	compatExtAttr       = 0x8
	compatResizeInode   = 0x10
	compatDirIndex      = 0x20
	compatLazyBG        = 0x40
	compatExcludeInode  = 0x80
	compatExcludeBitmap = 0x100
	compatSparseSuper2  = 0x200
	compatFastCommit    = 0x400
	compatStableInodes  = 0x800
	compatOrphanFile    = 0x1000

	roCompatSparseSuper   = 0x1
	roCompatLargeFile     = 0x2
	roCompatBtreeDir      = 0x4
	roCompatHugeFile      = 0x8
	roCompatGDTCsum       = 0x10
	roCompatDirNlink      = 0x20
	roCompatExtraIsize    = 0x40
	roCompatHasSnapshot   = 0x80
	roCompatQuota         = 0x100
	roCompatBigalloc      = 0x200
	roCompatMetadataCsum  = 0x400
	roCompatReplica       = 0x800
	roCompatReadonly      = 0x1000
	roCompatProject       = 0x2000
	roCompatSharedBlocks  = 0x4000
	roCompatVerity        = 0x8000
	roCompatOrphanPresent = 0x10000

	incompatCompression = 0x1
	incompatFiletype    = 0x2
	incompatRecover     = 0x4
	incompatJournalDev  = 0x8
	incompatMetaBG      = 0x10
	incompatExtents     = 0x40
	incompat64Bit       = 0x80
	incompatMMP         = 0x100
	incompatFlexBG      = 0x200
	incompatEAInode     = 0x400
	incompatDirData     = 0x1000
	incompatCsumSeed    = 0x2000
	incompatLargeDir    = 0x4000
	incompatInlineData  = 0x8000
	incompatEncrypt     = 0x10000
	incompatCasefold    = 0x20000
)

// Features is the feature matrix. Bits not listed here are unknown: an
// unknown incompat or ro_compat bit refuses the volume, an unknown compat
// bit refuses read-write.
var Features = [...]Feature{
	{"dir_prealloc", Compat, compatDirPrealloc, ReadWrite, "allocation hint; never acted on"},
	{"imagic_inodes", Compat, compatImagicInodes, ReadOnly, "AFS inodes are not maintained"},
	{"has_journal", Compat, compatHasJournal, ReadWrite, "internal journal only; required for read-write"},
	{"ext_attr", Compat, compatExtAttr, ReadWrite, "existing attributes are preserved"},
	{"resize_inode", Compat, compatResizeInode, ReadWrite, "reserved descriptor blocks are left untouched"},
	{"dir_index", Compat, compatDirIndex, ReadWrite, "hash-indexed directories"},
	{"lazy_bg", Compat, compatLazyBG, ReadOnly, "obsolete lazy group initialization"},
	{"exclude_inode", Compat, compatExcludeInode, Reject, "snapshot exclusion is not implemented"},
	{"exclude_bitmap", Compat, compatExcludeBitmap, Reject, "snapshot exclusion is not implemented"},
	{"sparse_super2", Compat, compatSparseSuper2, ReadOnly, "backup superblock placement is not maintained"},
	{"fast_commit", Compat, compatFastCommit, ReadOnly, "clean volumes only; fast commits are never replayed"},
	{"stable_inodes", Compat, compatStableInodes, ReadWrite, "inodes are never renumbered"},
	{"orphan_file", Compat, compatOrphanFile, ReadOnly, "clean volumes only; the orphan file is not maintained"},

	{"sparse_super", ROCompat, roCompatSparseSuper, ReadWrite, "backup superblocks in groups 0, 1 and powers of 3, 5, 7"},
	{"large_file", ROCompat, roCompatLargeFile, ReadWrite, "64-bit file sizes"},
	{"btree_dir", ROCompat, roCompatBtreeDir, Reject, "never implemented by Linux"},
	{"huge_file", ROCompat, roCompatHugeFile, ReadWrite, "block counts in filesystem-block units when flagged"},
	{"uninit_bg", ROCompat, roCompatGDTCsum, ReadOnly, "CRC16 descriptor checksums are verified, not written"},
	{"dir_nlink", ROCompat, roCompatDirNlink, ReadWrite, "directory link counts saturate at 1"},
	{"extra_isize", ROCompat, roCompatExtraIsize, ReadWrite, "extended inode fields"},
	{"snapshot", ROCompat, roCompatHasSnapshot, Reject, "snapshots are not implemented"},
	{"quota", ROCompat, roCompatQuota, ReadOnly, "quota files are not updated"},
	{"bigalloc", ROCompat, roCompatBigalloc, Reject, "cluster allocation is not implemented"},
	{"metadata_csum", ROCompat, roCompatMetadataCsum, ReadWrite, "CRC32C metadata checksums"},
	{"replica", ROCompat, roCompatReplica, Reject, "replicas are not implemented"},
	{"read-only", ROCompat, roCompatReadonly, ReadOnly, "the volume asks to be mounted read-only"},
	{"project", ROCompat, roCompatProject, ReadOnly, "project quota is not updated"},
	{"shared_blocks", ROCompat, roCompatSharedBlocks, ReadOnly, "shared blocks cannot be written in place"},
	{"verity", ROCompat, roCompatVerity, ReadOnly, "file data is read; verity metadata is not checked"},
	{"orphan_present", ROCompat, roCompatOrphanPresent, Reject, "orphan file cleanup is not implemented"},

	{"compression", Incompat, incompatCompression, Reject, "never implemented by Linux"},
	{"filetype", Incompat, incompatFiletype, ReadWrite, "file type in directory entries; required for read-write"},
	{"needs_recovery", Incompat, incompatRecover, ReadOnly, "mount state, not a format feature: replay first"},
	{"journal_dev", Incompat, incompatJournalDev, Reject, "external journal devices are not supported"},
	{"meta_bg", Incompat, incompatMetaBG, Reject, "meta_bg descriptor placement is not implemented"},
	{"extent", Incompat, incompatExtents, ReadWrite, "extent-mapped files; required for read-write"},
	{"64bit", Incompat, incompat64Bit, ReadWrite, "64-bit block numbers and wide descriptors"},
	{"mmp", Incompat, incompatMMP, Reject, "multi-mount protection is not implemented"},
	{"flex_bg", Incompat, incompatFlexBG, ReadWrite, "bitmaps and inode tables may sit in other groups"},
	{"ea_inode", Incompat, incompatEAInode, ReadOnly, "large attribute values in inodes are not written"},
	{"dirdata", Incompat, incompatDirData, Reject, "directory entry data is not implemented"},
	{"metadata_csum_seed", Incompat, incompatCsumSeed, ReadWrite, "checksum seed stored in the superblock"},
	{"large_dir", Incompat, incompatLargeDir, ReadOnly, "three-level hash trees are read, not built"},
	{"inline_data", Incompat, incompatInlineData, Reject, "inline data is not implemented"},
	{"encrypt", Incompat, incompatEncrypt, Reject, "encryption is not implemented"},
	{"casefold", Incompat, incompatCasefold, Reject, "case-insensitive names are not implemented"},
}

// FeatureSet is the three feature words of a superblock.
type FeatureSet struct {
	Compat   uint32
	Incompat uint32
	ROCompat uint32
}

func (set FeatureSet) word(class FeatureClass) uint32 {
	switch class {
	case Compat:
		return set.Compat
	case Incompat:
		return set.Incompat
	case ROCompat:
		return set.ROCompat
	}
	return 0
}

// Has reports whether the set contains the named matrix feature.
func (set FeatureSet) Has(name string) bool {
	for index := range Features {
		if Features[index].Name == name {
			return set.word(Features[index].Class)&Features[index].Mask != 0
		}
	}
	return false
}

// Negotiation is the outcome of matching a volume's features against the
// matrix.
type Negotiation struct {
	// Support is the most a mount may do: Reject, ReadOnly or ReadWrite.
	Support Support
	// Reason names the first feature (in matrix order, then unknown bits)
	// that lowered Support, or is empty for ReadWrite.
	Reason string
}

// requiredForWrite lists the features the writable profile cannot work
// without.
var requiredForWrite = [...]string{"has_journal", "extent", "filetype", "metadata_csum"}

// Negotiate matches set against the matrix. needs_recovery is a state, not
// a format feature, and the mount sequence handles it, so it is not
// considered here.
func Negotiate(set FeatureSet) Negotiation {
	result := Negotiation{Support: ReadWrite}
	lower := func(to Support, reason string) {
		if to < result.Support {
			result.Support, result.Reason = to, reason
		}
	}
	set.Incompat &^= incompatRecover
	for index := range Features {
		feature := &Features[index]
		if set.word(feature.Class)&feature.Mask == 0 {
			continue
		}
		switch feature.Support {
		case Reject:
			lower(Reject, feature.Class.String()+" feature "+feature.Name+": "+feature.Note)
		case ReadOnly:
			lower(ReadOnly, feature.Class.String()+" feature "+feature.Name+": "+feature.Note)
		}
	}
	if unknown := set.Incompat &^ knownMask(Incompat); unknown != 0 {
		lower(Reject, "unknown incompat feature bits "+hex32(unknown))
	}
	if unknown := set.ROCompat &^ knownMask(ROCompat); unknown != 0 {
		lower(Reject, "unknown ro_compat feature bits "+hex32(unknown))
	}
	if unknown := set.Compat &^ knownMask(Compat); unknown != 0 {
		lower(ReadOnly, "unknown compat feature bits "+hex32(unknown))
	}
	for _, name := range requiredForWrite {
		if !set.Has(name) {
			lower(ReadOnly, "read-write requires feature "+name)
		}
	}
	return result
}

func knownMask(class FeatureClass) uint32 {
	var mask uint32
	for index := range Features {
		if Features[index].Class == class {
			mask |= Features[index].Mask
		}
	}
	return mask
}

// Names lists the matrix names of the features in set, then any unknown
// bits as hexadecimal words.
func (set FeatureSet) Names() []string {
	var names []string
	for index := range Features {
		if set.word(Features[index].Class)&Features[index].Mask != 0 {
			names = append(names, Features[index].Name)
		}
	}
	for _, class := range [...]FeatureClass{Compat, Incompat, ROCompat} {
		if unknown := set.word(class) &^ knownMask(class); unknown != 0 {
			names = append(names, class.String()+":"+hex32(unknown))
		}
	}
	return names
}

func hex32(value uint32) string {
	const digits = "0123456789abcdef"
	var text [10]byte
	text[0], text[1] = '0', 'x'
	for index := 0; index < 8; index++ {
		text[9-index] = digits[value&0xf]
		value >>= 4
	}
	return string(text[:])
}
