package fat32

import "goos/kernel/ramfs"

// RepairCode identifies one on-disk correction made by Repair.
type RepairCode uint8

const (
	RepairPrimaryBoot RepairCode = iota + 1
	RepairBackupBoot
	RepairFATCopies
	RepairLFN
	RepairChain
	RepairLostCluster
	RepairEmptyFile
	RepairFSInfo
)

func (code RepairCode) String() string {
	switch code {
	case RepairPrimaryBoot:
		return "primary-boot"
	case RepairBackupBoot:
		return "backup-boot"
	case RepairFATCopies:
		return "fat-copies"
	case RepairLFN:
		return "lfn"
	case RepairChain:
		return "chain"
	case RepairLostCluster:
		return "lost-cluster"
	case RepairEmptyFile:
		return "empty-file"
	case RepairFSInfo:
		return "fsinfo"
	}
	return "unknown"
}

type RepairAction struct {
	Code    RepairCode
	Cluster uint32
	Entry   uint32
	Value   uint32
}

type RepairSink interface{ Repair(RepairAction) }

type RepairReport struct {
	After    CheckReport
	Repaired uint32
	Status   ramfs.Status
}

type repairer struct {
	fs       FileSystem
	geo      geometry
	owned    []byte
	jobs     []checkJob
	sink     RepairSink
	report   *RepairReport
	safeLost bool
}

// Repair applies only corrections whose intended result is unambiguous. The
// caller must hold an exclusive writable block slice. A final read-only check
// records every inconsistency deliberately left unresolved.
func Repair(device Device, issues CheckSink, repairs RepairSink) RepairReport {
	var report RepairReport
	geo, status := repairBoot(device, &report, repairs)
	if status != ramfs.StatusOK {
		report.Status = status
		return report
	}
	worker := repairer{geo: geo, owned: make([]byte, (geo.clusters+9)/8), jobs: []checkJob{{}}, sink: repairs, report: &report, safeLost: true}
	if _, ok := worker.fs.Mount(device); !ok {
		report.Status = worker.fs.Refused()
		return report
	}
	worker.walk()
	if worker.safeLost {
		worker.reclaimLost()
	}
	worker.syncFATCopies()
	worker.rewriteFSInfo()
	if !worker.fs.flushFAT() || !device.Flush() {
		report.Status = ramfs.StatusIOError
		return report
	}
	report.After = Check(device, issues)
	report.Status = report.After.Status
	return report
}

func repairBoot(device Device, report *RepairReport, sink RepairSink) (geometry, ramfs.Status) {
	var boot [unit]byte
	if device == nil || !device.ReadAt(0, boot[:]) {
		return geometry{}, ramfs.StatusIOError
	}
	capacity := device.Capacity()
	geo, status := parseBoot(boot[:], capacity)
	if status == ramfs.StatusCorrupt {
		backup := boot
		recovered, ok := recoverBoot(device, capacity, backup[:])
		if !ok {
			return geometry{}, status
		}
		sector := make([]byte, recovered.sectorSize)
		if !device.ReadAt(recovered.backup*recovered.sectorSize, sector) || !device.WriteAt(0, sector) {
			return geometry{}, ramfs.StatusIOError
		}
		geo, status = recovered, ramfs.StatusOK
		report.action(sink, RepairAction{Code: RepairPrimaryBoot})
	}
	if status != ramfs.StatusOK {
		return geometry{}, status
	}
	if geo.backup != 0 && checkBackup(device, boot[:], &geo, capacity) != BackupMatches {
		sector := make([]byte, geo.sectorSize)
		if !device.ReadAt(0, sector) || !device.WriteAt(geo.backup*geo.sectorSize, sector) {
			return geometry{}, ramfs.StatusIOError
		}
		report.action(sink, RepairAction{Code: RepairBackupBoot})
	}
	if !device.Flush() {
		return geometry{}, ramfs.StatusIOError
	}
	return geo, ramfs.StatusOK
}

func (repair *repairer) walk() {
	for len(repair.jobs) != 0 {
		job := repair.jobs[len(repair.jobs)-1]
		repair.jobs = repair.jobs[:len(repair.jobs)-1]
		dir, status := repair.fs.openDirectory(job.cluster)
		if status != ramfs.StatusOK || !repair.claim(dir.first, 0, false) {
			repair.safeLost = false
			continue
		}
		repair.fs.scan.reset()
		longStart := -1
		for index := 0; index < repair.fs.entries(&dir); index++ {
			entry, status := repair.fs.readEntry(&dir, index)
			if status != ramfs.StatusOK {
				repair.safeLost = false
				break
			}
			if entry[0] == entryEnd || entry[0] == entryDeleted {
				if longStart >= 0 {
					repair.deleteLFN(&dir, longStart, index)
				}
				repair.fs.scan.reset()
				longStart = -1
				if entry[0] == entryEnd {
					break
				}
				continue
			}
			if isLongEntry(entry) {
				if longStart < 0 {
					longStart = index
				}
				repair.fs.scan.long(entry, index)
				continue
			}
			if longStart >= 0 {
				var name [maxLongName]byte
				if _, ok := repair.fs.scan.complete(shortOf(entry), name[:]); !ok {
					repair.deleteLFN(&dir, longStart, index)
				}
			}
			repair.fs.scan.reset()
			longStart = -1
			if !listed(entry) || entry[0] == '.' {
				continue
			}
			first := entryCluster(entry)
			if isDirectory(entry) {
				repair.jobs = append(repair.jobs, checkJob{cluster: first, parent: job.cluster})
				continue
			}
			size := entrySizeOf(entry)
			if size == 0 && first != 0 {
				setEntryCluster(&entry, 0)
				if repair.fs.writeEntry(&dir, index, entry) == ramfs.StatusOK {
					repair.report.action(repair.sink, RepairAction{Code: RepairEmptyFile, Cluster: first, Entry: uint32(index)})
				} else {
					repair.safeLost = false
				}
				continue
			}
			if size != 0 && !repair.claim(first, uint32(repair.fs.clustersFor(size)), true) {
				repair.safeLost = false
			}
		}
	}
}

func (repair *repairer) claim(first, want uint32, trim bool) bool {
	count := uint32(0)
	for cluster := first; ; {
		if !repair.fs.valid(cluster) || bitSet(repair.owned, cluster) {
			return false
		}
		repair.owned[cluster/8] |= 1 << (cluster % 8)
		count++
		value, status := repair.fs.fat(cluster)
		if status != ramfs.StatusOK {
			return false
		}
		if trim && count == want && value < endOfChain {
			if repair.fs.setFAT(cluster, endOfChainMark) != ramfs.StatusOK {
				return false
			}
			repair.report.action(repair.sink, RepairAction{Code: RepairChain, Cluster: cluster, Value: value})
			return true
		}
		if value >= endOfChain {
			return want == 0 || count >= want
		}
		if !repair.fs.valid(value) {
			return false
		}
		cluster = value
	}
}

func (repair *repairer) deleteLFN(dir *directory, first, end int) {
	for index := first; index < end; index++ {
		entry, status := repair.fs.readEntry(dir, index)
		if status != ramfs.StatusOK || !isLongEntry(entry) {
			continue
		}
		entry[0] = entryDeleted
		if repair.fs.writeEntry(dir, index, entry) == ramfs.StatusOK {
			repair.report.action(repair.sink, RepairAction{Code: RepairLFN, Cluster: dir.first, Entry: uint32(index)})
		}
	}
}

func (repair *repairer) reclaimLost() {
	for cluster := uint32(2); cluster < repair.fs.clusters+2; cluster++ {
		value, status := repair.fs.fat(cluster)
		if status != ramfs.StatusOK {
			return
		}
		if value != 0 && value != badCluster && !bitSet(repair.owned, cluster) && repair.fs.setFAT(cluster, 0) == ramfs.StatusOK {
			repair.report.action(repair.sink, RepairAction{Code: RepairLostCluster, Cluster: cluster, Value: value})
		}
	}
}

func (repair *repairer) syncFATCopies() {
	if repair.fs.fatCopies != repair.fs.fatTotal || repair.fs.fatTotal < 2 || !repair.fs.flushFAT() {
		return
	}
	source := repair.fs.fatStart + uint64(repair.fs.fatRead)*repair.fs.fatSize
	var other [zeroSize]byte
	for copy := uint32(0); copy < repair.fs.fatTotal; copy++ {
		if copy == repair.fs.fatRead {
			continue
		}
		changed := false
		for offset := uint64(0); offset < repair.fs.fatSize; offset += uint64(len(repair.fs.scratch)) {
			chunk := min(uint64(len(repair.fs.scratch)), repair.fs.fatSize-offset)
			target := repair.fs.fatStart + uint64(copy)*repair.fs.fatSize + offset
			if !repair.fs.device.ReadAt(source+offset, repair.fs.scratch[:chunk]) || !repair.fs.device.ReadAt(target, other[:chunk]) {
				return
			}
			same := true
			for index := uint64(0); index < chunk; index++ {
				if repair.fs.scratch[index] != other[index] {
					same = false
					break
				}
			}
			if !same {
				if !repair.fs.device.WriteAt(target, repair.fs.scratch[:chunk]) {
					return
				}
				changed = true
			}
		}
		if changed {
			repair.report.action(repair.sink, RepairAction{Code: RepairFATCopies, Value: copy})
		}
	}
}

func (repair *repairer) rewriteFSInfo() {
	if !repair.fs.flushFAT() {
		return
	}
	free, next := uint32(0), fsInfoUnknown
	for cluster := uint32(2); cluster < repair.fs.clusters+2; cluster++ {
		value, status := repair.fs.fat(cluster)
		if status != ramfs.StatusOK {
			return
		}
		if value == 0 {
			free++
			if next == fsInfoUnknown {
				next = cluster
			}
		}
	}
	var info [unit]byte
	put32(info[0:], fsInfoLead)
	put32(info[fsInfoStructAt:], fsInfoStruct)
	put32(info[fsInfoFreeCount:], free)
	put32(info[fsInfoNextFree:], next)
	put32(info[fsInfoTrailAt:], fsInfoTrail)
	written := false
	for _, sector := range [2]uint64{repair.geo.fsInfo, repair.geo.backupFSInfo} {
		if sector == 0 {
			continue
		}
		var current [unit]byte
		offset := sector * repair.geo.sectorSize
		same := repair.fs.device.ReadAt(offset, current[:])
		for index := range current {
			if current[index] != info[index] {
				same = false
				break
			}
		}
		if !same && repair.fs.device.WriteAt(offset, info[:]) {
			written = true
		}
	}
	if written {
		repair.report.action(repair.sink, RepairAction{Code: RepairFSInfo, Value: free})
	}
}

func (report *RepairReport) action(sink RepairSink, action RepairAction) {
	report.Repaired++
	if sink != nil {
		sink.Repair(action)
	}
}
