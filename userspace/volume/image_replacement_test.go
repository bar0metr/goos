package volume

import (
	"encoding/binary"
	"testing"

	"goos/kernel/vfs"
	"goos/userspace/fsdetect"
	"goos/userspace/partition"
)

type replacementImage []byte

func (image replacementImage) ReadAt(offset uint64, target []byte) bool {
	if offset > uint64(len(image)) || uint64(len(target)) > uint64(len(image))-offset {
		return false
	}
	copy(target, image[offset:offset+uint64(len(target))])
	return true
}

func (image replacementImage) WriteAt(offset uint64, data []byte) bool {
	if offset > uint64(len(image)) || uint64(len(data)) > uint64(len(image))-offset {
		return false
	}
	copy(image[offset:offset+uint64(len(data))], data)
	return true
}

func ext4ReplacementImage(t *testing.T, partitionByte byte) replacementImage {
	t.Helper()
	const capacity = 16 << 20
	image := make(replacementImage, capacity)
	diskGUID, _ := partition.ParseGUID("11111111-1111-1111-1111-111111111111")
	typeGUID, _ := partition.ParseGUID("0FC63DAF-8483-4772-8E79-3D69D8477DE4")
	var partitionGUID partition.GUID
	partitionGUID[0] = partitionByte
	if !partition.WriteGPT(image, capacity, 512, diskGUID, []partition.GPTPartition{{
		Type: typeGUID, Unique: partitionGUID, First: 2048, Last: 16383, Name: "data",
	}}) {
		t.Fatal("write GPT")
	}
	superblock := image[2048*512+1024 : 2048*512+2048]
	binary.LittleEndian.PutUint32(superblock[0x00:], 1024)
	binary.LittleEndian.PutUint32(superblock[0x04:], 4096)
	binary.LittleEndian.PutUint32(superblock[0x18:], 0)
	binary.LittleEndian.PutUint32(superblock[0x20:], 8192)
	binary.LittleEndian.PutUint32(superblock[0x28:], 1024)
	binary.LittleEndian.PutUint16(superblock[0x38:], 0xef53)
	binary.LittleEndian.PutUint32(superblock[0x4c:], 1)
	for index := 0; index < 16; index++ {
		superblock[0x68+index] = byte(index + 1)
	}
	return image
}

func probeReplacementImage(t *testing.T, image replacementImage) (fsdetect.Result, partition.Identity) {
	t.Helper()
	var scanner partition.Scanner
	var table partition.Table
	if status := scanner.Scan(image, uint64(len(image)), 512, &table); status != partition.StatusOK {
		t.Fatalf("scan: %v", status)
	}
	selector, _ := partition.NameSelector("data")
	selected, status := partition.Select(&table, &selector)
	if status != partition.StatusOK {
		t.Fatalf("select: %v", status)
	}
	window := make([]byte, fsdetect.Window)
	if !image.ReadAt(selected.Start, window) {
		t.Fatal("read probe window")
	}
	return fsdetect.Probe(window, selected.Length), partition.IdentityOf(&table, selected)
}

func TestImageReplacementBetweenFailureAndRecovery(t *testing.T) {
	firstProbe, firstPhysical := probeReplacementImage(t, ext4ReplacementImage(t, 1))
	replacementProbe, replacementPhysical := probeReplacementImage(t, ext4ReplacementImage(t, 2))
	if !firstProbe.SameVolume(&replacementProbe) {
		t.Fatal("fixture images do not reuse the ext4 UUID")
	}
	if firstPhysical.Same(replacementPhysical) {
		t.Fatal("fixture images do not represent distinct partitions")
	}

	var table Table
	slot, status := table.Reserve(request(t, "ext4", "name:data", "/mnt/data"))
	if status != OK || table.Probed(slot, firstProbe, firstPhysical) != OK {
		t.Fatalf("first mount: slot=%d status=%v", slot, status)
	}
	generation := table.Launching(slot)
	if table.Attached(slot, generation, vfs.StatusOK) != OK || !table.Lost(slot, generation) {
		t.Fatal("failed to enter recovery")
	}
	if status := table.Probed(slot, replacementProbe, replacementPhysical); status != IdentityMismatch || table.Slot(slot).State != Offline {
		t.Fatalf("replacement image entered the old mount: %v", status)
	}
}
