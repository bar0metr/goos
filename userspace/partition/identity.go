package partition

// Identity pins the partition-table object that a selector resolved. It is
// independent of the filesystem UUID stored inside the partition.
type Identity struct {
	Scheme        Scheme
	DiskGUID      GUID
	PartitionGUID GUID
	Start         uint64
	Length        uint64
	Index         uint32
	MBRSignature  uint32
	MBRType       byte
}

// IdentityOf describes a selected partition. Additional volumes never select
// a whole device, so raw and table-less devices have no physical identity.
func IdentityOf(table *Table, volume Volume) Identity {
	if table == nil || volume.Index == 0 {
		return Identity{}
	}
	for index := 0; index < table.Count; index++ {
		entry := &table.Partitions[index]
		if entry.Index != volume.Index || entry.Start != volume.Start || entry.Length != volume.Length {
			continue
		}
		identity := Identity{
			Scheme: table.Scheme, DiskGUID: table.DiskGUID, PartitionGUID: entry.Unique,
			Start: entry.Start, Length: entry.Length, Index: entry.Index,
		}
		if table.Scheme == SchemeMBR {
			identity.DiskGUID, identity.PartitionGUID = GUID{}, GUID{}
			identity.MBRSignature, identity.MBRType = table.MBRSignature, entry.MBRType
		}
		return identity
	}
	return Identity{}
}

// Valid reports whether the identity can distinguish this partition from the
// same selector resolving on another disk image.
func (identity Identity) Valid() bool {
	if identity.Start == 0 || identity.Length == 0 || identity.Index == 0 {
		return false
	}
	switch identity.Scheme {
	case SchemeGPT:
		return identity.DiskGUID != (GUID{}) && identity.PartitionGUID != (GUID{}) && identity.MBRSignature == 0 && identity.MBRType == 0
	case SchemeMBR:
		return identity.MBRSignature != 0 && identity.DiskGUID == (GUID{}) && identity.PartitionGUID == (GUID{}) && identity.MBRType != 0
	default:
		return false
	}
}

// Same reports whether two valid identities name the same physical partition.
func (identity Identity) Same(other Identity) bool {
	return identity.Valid() && other.Valid() && identity == other
}
