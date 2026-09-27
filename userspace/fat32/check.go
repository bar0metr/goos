package fat32

import "goos/kernel/ramfs"

// CheckCode identifies one class of filesystem inconsistency. Check never
// changes the device; repair policy belongs to Phase 5.
type CheckCode uint8

const (
	CheckUnreadable CheckCode = iota + 1
	CheckBootRecovered
	CheckBackupBoot
	CheckFSInfo
	CheckFATCopies
	CheckInvalidChain
	CheckCrossLinked
	CheckChainTooShort
	CheckChainTooLong
	CheckLostCluster
	CheckDirectory
	CheckDot
	CheckDotDot
	CheckLFN
	CheckEmptyFileCluster
	CheckDuplicateName
	CheckAttributes
)

func (code CheckCode) String() string {
	switch code {
	case CheckUnreadable:
		return "unreadable"
	case CheckBootRecovered:
		return "boot-recovered"
	case CheckBackupBoot:
		return "backup-boot"
	case CheckFSInfo:
		return "fsinfo"
	case CheckFATCopies:
		return "fat-copies"
	case CheckInvalidChain:
		return "invalid-chain"
	case CheckCrossLinked:
		return "cross-linked"
	case CheckChainTooShort:
		return "chain-too-short"
	case CheckChainTooLong:
		return "chain-too-long"
	case CheckLostCluster:
		return "lost-cluster"
	case CheckDirectory:
		return "directory"
	case CheckDot:
		return "dot"
	case CheckDotDot:
		return "dot-dot"
	case CheckLFN:
		return "lfn"
	case CheckEmptyFileCluster:
		return "empty-file-cluster"
	case CheckDuplicateName:
		return "duplicate-name"
	case CheckAttributes:
		return "attributes"
	}
	return "unknown"
}

// CheckIssue is machine-readable and small enough to stream. Cluster is the
// affected cluster when known; Entry is the directory slot when known.
type CheckIssue struct {
	Code    CheckCode
	Cluster uint32
	Entry   uint32
	Value   uint32
}

type CheckSink interface{ Issue(CheckIssue) }

type CheckReport struct {
	Issues, Files, Directories, UsedClusters, FreeClusters uint32
	Status                                                 ramfs.Status
}

type checkJob struct{ cluster, parent uint32 }

// Check performs a read-only whole-volume consistency pass. The caller must
// hold an exclusive block slice or otherwise guarantee the volume is not
// mounted writable while the pass runs.
func Check(device Device, sink CheckSink) CheckReport {
	var report CheckReport
	var fs FileSystem
	if _, ok := fs.Mount(device); !ok {
		report.Status = fs.refusal
		report.add(sink, CheckIssue{Code: CheckUnreadable, Value: uint32(report.Status)})
		return report
	}
	report.Status = ramfs.StatusOK
	health := fs.Health()
	if health.Boot != BootPrimary {
		report.add(sink, CheckIssue{Code: CheckBootRecovered})
	}
	if health.Backup == BackupDiffers || health.Backup == BackupInvalid {
		report.add(sink, CheckIssue{Code: CheckBackupBoot, Value: uint32(health.Backup)})
	}
	if health.FSInfo == FSInfoInvalid || health.BackupFSInfo == BackupFSInfoDiffers || health.BackupFSInfo == BackupFSInfoInvalid {
		report.add(sink, CheckIssue{Code: CheckFSInfo})
	}
	if fs.fatCopies > 1 {
		mismatch, status := fs.CompareFATs()
		if status != ramfs.StatusOK {
			report.add(sink, CheckIssue{Code: CheckUnreadable})
		} else if mismatch.Entries != 0 {
			value := uint32(mismatch.Entries)
			if mismatch.Entries > uint64(^uint32(0)) {
				value = ^uint32(0)
			}
			report.add(sink, CheckIssue{Code: CheckFATCopies, Cluster: mismatch.First, Value: value})
		}
	}
	owned := make([]byte, (uint64(fs.clusters)+9)/8)
	jobs := []checkJob{{cluster: 0, parent: 0}}
	for len(jobs) != 0 {
		job := jobs[len(jobs)-1]
		jobs = jobs[:len(jobs)-1]
		dir, status := fs.openDirectory(job.cluster)
		if status != ramfs.StatusOK {
			report.add(sink, CheckIssue{Code: CheckDirectory, Cluster: job.cluster, Value: uint32(status)})
			continue
		}
		if !checkClaim(&fs, owned, dir.first, 0, false, &report, sink) {
			continue
		}
		report.Directories++
		fs.scan.reset()
		sawLong := false
		for index := 0; index < fs.entries(&dir); index++ {
			entry, status := fs.readEntry(&dir, index)
			if status != ramfs.StatusOK {
				report.add(sink, CheckIssue{Code: CheckUnreadable, Cluster: dir.first, Entry: uint32(index)})
				break
			}
			if entry[0] == entryEnd {
				if sawLong {
					report.add(sink, CheckIssue{Code: CheckLFN, Cluster: dir.first, Entry: uint32(index)})
				}
				break
			}
			if entry[0] == entryDeleted {
				if sawLong {
					report.add(sink, CheckIssue{Code: CheckLFN, Cluster: dir.first, Entry: uint32(index)})
				}
				fs.scan.reset()
				sawLong = false
				continue
			}
			if isLongEntry(entry) {
				fs.scan.long(entry, index)
				sawLong = true
				continue
			}
			if sawLong {
				var name [maxLongName]byte
				if _, ok := fs.scan.complete(shortOf(entry), name[:]); !ok {
					report.add(sink, CheckIssue{Code: CheckLFN, Cluster: dir.first, Entry: uint32(index)})
				}
			}
			fs.scan.reset()
			sawLong = false
			if entry[11]&attrVolume != 0 {
				if entry[11]&attrDirectory != 0 || entryCluster(entry) != 0 || entrySizeOf(entry) != 0 {
					report.add(sink, CheckIssue{Code: CheckAttributes, Cluster: dir.first, Entry: uint32(index)})
				}
				continue
			}
			first := entryCluster(entry)
			if entry[0] == '.' {
				if job.cluster == 0 || index > 1 {
					report.add(sink, CheckIssue{Code: CheckDot, Cluster: dir.first, Entry: uint32(index)})
				} else if index == 0 && (!sameShort(entry, dotName) || first != job.cluster) {
					report.add(sink, CheckIssue{Code: CheckDot, Cluster: dir.first, Entry: uint32(index), Value: first})
				} else if index == 1 && (!sameShort(entry, dotDotName) || first != job.parent && (job.parent != 0 || first != fs.rootCluster)) {
					report.add(sink, CheckIssue{Code: CheckDotDot, Cluster: dir.first, Entry: uint32(index), Value: first})
				}
				continue
			}
			if !listed(entry) {
				continue
			}
			for previous := 0; previous < index; previous++ {
				other, status := fs.readEntry(&dir, previous)
				if status != ramfs.StatusOK {
					break
				}
				if listed(other) && sameShort(other, shortOf(entry)) {
					report.add(sink, CheckIssue{Code: CheckDuplicateName, Cluster: dir.first, Entry: uint32(index)})
					break
				}
			}
			if isDirectory(entry) {
				jobs = append(jobs, checkJob{cluster: first, parent: job.cluster})
				continue
			}
			report.Files++
			size := entrySizeOf(entry)
			if size == 0 {
				if first != 0 {
					report.add(sink, CheckIssue{Code: CheckEmptyFileCluster, Cluster: first, Entry: uint32(index)})
				}
				continue
			}
			checkClaim(&fs, owned, first, uint32(fs.clustersFor(size)), true, &report, sink)
		}
	}
	for cluster := uint32(2); cluster < fs.clusters+2; cluster++ {
		value, status := fs.fat(cluster)
		if status != ramfs.StatusOK {
			report.add(sink, CheckIssue{Code: CheckUnreadable, Cluster: cluster})
			break
		}
		if value == 0 {
			report.FreeClusters++
		} else if !bitSet(owned, cluster) {
			report.add(sink, CheckIssue{Code: CheckLostCluster, Cluster: cluster, Value: value})
		}
	}
	if fs.recordedFree != fsInfoUnknown && fs.recordedFree != report.FreeClusters {
		report.add(sink, CheckIssue{Code: CheckFSInfo, Value: fs.recordedFree})
	}
	return report
}

func (report *CheckReport) add(sink CheckSink, issue CheckIssue) {
	report.Issues++
	if sink != nil {
		sink.Issue(issue)
	}
}

func bitSet(bits []byte, cluster uint32) bool { return bits[cluster/8]&(1<<(cluster%8)) != 0 }

func checkClaim(fs *FileSystem, owned []byte, first, want uint32, exact bool, report *CheckReport, sink CheckSink) bool {
	count := uint32(0)
	for cluster := first; ; {
		if !fs.valid(cluster) {
			report.add(sink, CheckIssue{Code: CheckInvalidChain, Cluster: cluster})
			return false
		}
		if bitSet(owned, cluster) {
			report.add(sink, CheckIssue{Code: CheckCrossLinked, Cluster: cluster})
			return false
		}
		owned[cluster/8] |= 1 << (cluster % 8)
		report.UsedClusters++
		count++
		value, status := fs.fat(cluster)
		if status != ramfs.StatusOK {
			report.add(sink, CheckIssue{Code: CheckUnreadable, Cluster: cluster})
			return false
		}
		if value >= endOfChain {
			break
		}
		if !fs.valid(value) {
			report.add(sink, CheckIssue{Code: CheckInvalidChain, Cluster: cluster, Value: value})
			return false
		}
		cluster = value
	}
	if want != 0 && count < want {
		report.add(sink, CheckIssue{Code: CheckChainTooShort, Cluster: first, Value: count})
	} else if exact && count > want {
		report.add(sink, CheckIssue{Code: CheckChainTooLong, Cluster: first, Value: count})
	}
	return true
}
