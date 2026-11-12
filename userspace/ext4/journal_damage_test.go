package ext4

import (
	"testing"
)

// damageRun commits a few transactions on a crash device: a file with an
// extent leaf, its unlink (which revokes the logged leaf), then two
// creates, and leaves the volume mounted. It returns the device and the
// classes of its events.
func damageRun(t *testing.T) (*crashDevice, []string) {
	device := newCrashDevice(crashBase(t))
	fs := ok(mountScratch(device, testClock()))
	file := ok(fs.Create(root(t, fs), []byte("leafy"), ModeRegular|0o644, 0, 0))
	for index := 0; index < 200; index += 20 {
		if _, err := fs.WriteFile(file, uint64(index)*8192, []byte{1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := fs.Unlink(root(t, fs), []byte("leafy")); err != nil {
		t.Fatal(err)
	}
	ok(fs.Create(root(t, fs), []byte("one"), ModeRegular|0o644, 0, 0))
	ok(fs.Create(root(t, fs), []byte("two"), ModeRegular|0o644, 0, 0))
	return device, device.classify()
}

// eventsOf lists the indexes of the events of class.
func eventsOf(classes []string, class string) []int {
	var found []int
	for index, got := range classes {
		if got == class {
			found = append(found, index)
		}
	}
	return found
}

// reseal recomputes the checksum of a descriptor, revoke or commit block.
func reseal(log *journalLog, data []byte) {
	if be32(data, 4) == jbd2Commit {
		putBE32(data[0x10:], log.blockChecksum(data, 0x10))
		return
	}
	log.sealTail(data)
}

// TestReplayRefusesDamagedCommittedTransaction damages one block of a
// transaction that committed and that later transactions follow. Each
// damage stops the scan early; replay must refuse the volume instead of
// treating the rest of the log as an unfinished tail.
func TestReplayRefusesDamagedCommittedTransaction(t *testing.T) {
	device, classes := damageRun(t)
	log := device.log
	descriptors := eventsOf(classes, classDescriptor)
	commits := eventsOf(classes, classCommit)
	revokes := eventsOf(classes, classRevoke)
	if len(revokes) == 0 || len(commits) < 3 {
		t.Fatalf("workload wrote %d revoke blocks and %d commits", len(revokes), len(commits))
	}
	firstDescriptor, firstCommit := device.events[descriptors[0]], device.events[commits[0]]
	cases := []struct {
		name   string
		event  crashEvent
		damage func(data []byte)
	}{
		{"descriptor checksum", firstDescriptor, func(data []byte) { data[jbd2HeaderBytes+1] ^= 0x40 }},
		{"revoke checksum", device.events[revokes[0]], func(data []byte) { data[jbd2RevokeHeader] ^= 0x40 }},
		{"commit checksum", firstCommit, func(data []byte) { data[0x30] ^= 0x40 }},
		{"sequence jump in a commit block", firstCommit, func(data []byte) {
			putBE32(data[8:], be32(data, 8)+5)
			reseal(log, data)
		}},
		{"descriptor without a last tag", firstDescriptor, func(data []byte) {
			size := log.tagBytes()
			for offset := jbd2HeaderBytes; offset+size <= len(data)-log.tailBytes(); {
				tag := log.decodeTag(data[offset:])
				if tag.flags&jbd2FlagLastTag != 0 {
					putBE32(data[offset+4:], tag.flags&^jbd2FlagLastTag)
					break
				}
				offset += size
				if tag.flags&jbd2FlagSameUUID == 0 {
					offset += jbd2UUIDBytes
				}
			}
			reseal(log, data)
		}},
	}
	for _, test := range cases {
		image := append([]byte(nil), device.current...)
		block := append([]byte(nil), test.event.data...)
		test.damage(block)
		copy(image[test.event.offset:], block)
		_, err := mountScratch(&scratchDevice{data: image}, testClock())
		if KindOf(err) != KindCorrupt {
			t.Fatalf("%s: recovery returned %v, want corrupt metadata", test.name, err)
		}
	}
}

// TestReplayEndsAtTornCommit tears the last transaction's commit block:
// that transaction never committed, so recovery drops it and keeps every
// earlier one.
func TestReplayEndsAtTornCommit(t *testing.T) {
	device, classes := damageRun(t)
	commits := eventsOf(classes, classCommit)
	last := commits[len(commits)-1]
	image := device.landed(last)
	commit := device.events[last]
	// The log block held something else before; only the first sector of
	// the commit write landed.
	for index := 512; index < len(commit.data); index++ {
		image[commit.offset+uint64(index)] = 0xA5
	}
	copy(image[commit.offset:], commit.data[:512])
	_, fs := recoverAt(t, image)
	lookupInode(t, fs, root(t, fs), "one")
	if _, _, err := fs.Lookup(root(t, fs), []byte("two")); KindOf(err) != KindNotFound {
		t.Fatalf("uncommitted create survived: %v", err)
	}
}

// TestJournalFeatureValidation refuses journal superblocks Linux refuses:
// a read-only compatible feature, and checksum v2 with v3.
func TestJournalFeatureValidation(t *testing.T) {
	base := crashBase(t)
	log, blockSize := bootstrapLog(base)
	edit := func(change func(raw []byte)) []byte {
		image := append([]byte(nil), base...)
		raw := image[log.superBlock*blockSize:]
		change(raw)
		crc := crc32c(^uint32(0), raw[:0xFC])
		crc = crc32cZeros(crc, 4)
		crc = crc32c(crc, raw[0x100:jbd2SuperblockBytes])
		putBE32(raw[0xFC:], crc)
		return image
	}
	roCompat := edit(func(raw []byte) { putBE32(raw[0x2C:], 1) })
	if _, err := mountScratch(&scratchDevice{data: roCompat}, testClock()); KindOf(err) != KindUnsupported {
		t.Fatalf("journal with ro_compat bits: %v", err)
	}
	both := edit(func(raw []byte) { putBE32(raw[0x28:], be32(raw, 0x28)|jbd2IncompatCsumV2|jbd2IncompatCsumV3) })
	if _, err := mountScratch(&scratchDevice{data: both}, testClock()); KindOf(err) != KindCorrupt {
		t.Fatalf("writable mount of a journal with checksum v2 and v3: %v", err)
	}
	if _, err := Mount(&scratchDevice{data: both}, Options{}); KindOf(err) != KindCorrupt {
		t.Fatalf("read-only mount of a journal with checksum v2 and v3: %v", err)
	}
}

// TestTornSuperblockReplayed tears a home write of the superblock between
// its two sectors. The checksum fails, the bootstrap still finds the
// journal, and replay writes the committed copy back.
func TestTornSuperblockReplayed(t *testing.T) {
	device, classes := damageRun(t)
	supers := eventsOf(classes, classSuper)
	last := supers[len(supers)-1]
	image := device.landed(last)
	write := device.events[last]
	copy(image[write.offset:], write.data[:superblockOffset+512])
	if _, err := probe(&scratchDevice{data: image}, true); KindOf(err) != KindCorrupt {
		t.Fatalf("the tear left a valid superblock checksum: %v", err)
	}
	recovered, fs := recoverAt(t, image)
	lookupInode(t, fs, root(t, fs), "two")
	checkVolume(t, recovered)
}

// TestTornSuperblockWithoutJournalRefused damages the superblock checksum
// of a cleanly unmounted volume: no log holds a copy, so the writable
// mount refuses it.
func TestTornSuperblockWithoutJournalRefused(t *testing.T) {
	image := crashBase(t)
	image[superblockOffset+0xC] ^= 1
	_, err := mountScratch(&scratchDevice{data: image}, testClock())
	if KindOf(err) != KindCorrupt {
		t.Fatalf("writable mount of a damaged superblock: %v", err)
	}
}
