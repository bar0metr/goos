package ext4

const (
	xattrMagic       = 0xEA020000
	xattrBlockHeader = 32
	xattrEntryHeader = 16
	// maxXattrEntries bounds one attribute area; a 4 KiB block holds at
	// most 4096/20 entries.
	maxXattrEntries = 256
)

var xattrPrefixes = [...]string{
	1: "user.",
	2: "system.posix_acl_access",
	3: "system.posix_acl_default",
	4: "trusted.",
	6: "security.",
	7: "system.",
	8: "system.richacl",
}

// Xattr is one extended attribute: its full name (prefix included) and
// value, both copied out of the volume.
type Xattr struct {
	Name  []byte
	Value []byte
	// InodeValue is set when the value lives in a separate inode
	// (ea_inode); Value is then empty.
	InodeValue uint32
}

// Xattrs returns the attributes of inode, in-inode ones first. Unknown name
// prefixes are refused rather than dropped, so a caller that rewrites the
// set never loses one silently.
func (fs *FS) Xattrs(inode *Inode) ([]Xattr, error) {
	var out []Xattr
	if fs.sb.Features.Compat&compatExtAttr == 0 {
		return nil, nil
	}
	start := goodOldIsize + int(inode.ExtraIsize)
	if len(inode.raw) > goodOldIsize && start+4 <= len(inode.raw) && le32(inode.raw, start) == xattrMagic {
		area := inode.raw[start+4:]
		var err error
		if out, err = parseXattrs(area, area, out); err != nil {
			return nil, err
		}
	}
	if inode.FileACL != 0 {
		if inode.FileACL <= uint64(fs.sb.FirstDataBlock) || inode.FileACL >= fs.sb.BlocksCount {
			return nil, corrupt("attribute block outside the volume")
		}
		data, err := fs.cache.get(inode.FileACL)
		if err != nil {
			return nil, err
		}
		if le32(data, 0) != xattrMagic || le32(data, 8) != 1 {
			return nil, corrupt("attribute block header invalid")
		}
		if fs.sb.HasMetadataCsum() {
			var number [8]byte
			putLE32(number[:4], uint32(inode.FileACL))
			putLE32(number[4:], uint32(inode.FileACL>>32))
			crc := crc32c(fs.seed, number[:])
			crc = crc32c(crc, data[:0x10])
			crc = crc32cZeros(crc, 4)
			crc = crc32c(crc, data[0x14:])
			if crc != le32(data, 0x10) {
				return nil, corrupt("attribute block checksum mismatch")
			}
		}
		if out, err = parseXattrs(data[xattrBlockHeader:], data, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// parseXattrs decodes the entry list at entries. Value offsets count from
// base: the first entry for in-inode attributes, the block start for a
// block.
func parseXattrs(entries []byte, base []byte, out []Xattr) ([]Xattr, error) {
	offset := 0
	for count := 0; ; count++ {
		if count > maxXattrEntries {
			return nil, corrupt("too many attribute entries")
		}
		if offset+4 > len(entries) {
			return nil, corrupt("attribute list not terminated")
		}
		if le32(entries, offset) == 0 {
			return out, nil
		}
		if offset+xattrEntryHeader > len(entries) {
			return nil, corrupt("attribute entry truncated")
		}
		nameLen := int(entries[offset])
		index := int(entries[offset+1])
		valueOffset := int(le16(entries, offset+2))
		valueInode := le32(entries, offset+4)
		valueSize := int(le32(entries, offset+8))
		if nameLen == 0 || offset+xattrEntryHeader+nameLen > len(entries) {
			return nil, corrupt("attribute name truncated")
		}
		if index >= len(xattrPrefixes) || (xattrPrefixes[index] == "" && index != 0) {
			return nil, fail(KindUnsupported, "attribute name prefix unknown")
		}
		name := make([]byte, 0, len(xattrPrefixes[index])+nameLen)
		name = append(name, xattrPrefixes[index]...)
		name = append(name, entries[offset+xattrEntryHeader:offset+xattrEntryHeader+nameLen]...)
		attr := Xattr{Name: name, InodeValue: valueInode}
		if valueInode == 0 {
			if valueSize > len(base) || valueOffset > len(base)-valueSize {
				return nil, corrupt("attribute value outside its area")
			}
			attr.Value = append([]byte(nil), base[valueOffset:valueOffset+valueSize]...)
		}
		out = append(out, attr)
		offset += (xattrEntryHeader + nameLen + 3) &^ 3
	}
}
