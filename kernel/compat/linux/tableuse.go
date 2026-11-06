package linux

// TableUse is how many slots the personality's shared tables hold. Every
// field counts against a fixed limit: MaxOpenDescriptions,
// MaxOpenDirectories, MaxEventCounters, MaxTerminals, and for processes
// LinuxGlobalMaxProcesses, with LinuxFamilyMaxProcesses for one family.
type TableUse struct {
	Descriptions, Directories, EventCounters, Terminals int
	// Processes counts Linux process records, zombies included;
	// BusiestFamily is the most one family holds.
	Processes, BusiestFamily int
}

// Tables reports the use of the shared tables at one instant.
func Tables() TableUse {
	tablesLock.Lock()
	defer tablesLock.Unlock()
	var use TableUse
	use.Descriptions, _ = openDescriptionCountHeld()
	for index := range directorySlots {
		if directorySlots[index].owner != 0 {
			use.Directories++
		}
	}
	for index := range eventCounters {
		if eventCounters[index].familyID != 0 {
			use.EventCounters++
		}
	}
	for index := range terminals {
		if terminals[index].live {
			use.Terminals++
		}
	}
	for index := range processRecords {
		record := &processRecords[index]
		if record.State == 0 {
			continue
		}
		use.Processes++
		// A family is counted at its first record only.
		first := true
		for earlier := 0; earlier < index; earlier++ {
			if processRecords[earlier].State != 0 && processRecords[earlier].FamilyID == record.FamilyID {
				first = false
				break
			}
		}
		if first {
			_, family := processCounts(record.FamilyID)
			use.BusiestFamily = max(use.BusiestFamily, family)
		}
	}
	return use
}
