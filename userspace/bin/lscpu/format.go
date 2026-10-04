package main

import "goos/internal/cpuinfo"

type cpuData struct {
	summary    cpuinfo.Summary
	entries    [256]cpuinfo.CPUEntry
	entryCount int
	caches     [cpuinfo.MaxCaches]cpuinfo.CacheInfo
	cacheCount int
}

type output struct{ data []byte }

func (out *output) text(value string)  { out.data = append(out.data, value...) }
func (out *output) bytes(value []byte) { out.data = append(out.data, value...) }
func (out *output) byte(value byte)    { out.data = append(out.data, value) }
func (out *output) uint(value uint64) {
	var digits [20]byte
	position := len(digits)
	for {
		position--
		digits[position] = byte(value%10) + '0'
		value /= 10
		if value == 0 {
			break
		}
	}
	out.bytes(digits[position:])
}
func (out *output) hex(value uint64) {
	const digits = "0123456789abcdef"
	started := false
	for shift := 60; shift >= 0; shift -= 4 {
		nibble := byte(value >> uint(shift) & 0xf)
		if nibble != 0 || started || shift == 0 {
			started = true
			out.byte(digits[nibble])
		}
	}
}
func (out *output) field(label string) {
	out.text(label)
	for column := len(label); column < 26; column++ {
		out.byte(' ')
	}
}
func (out *output) line(label string, value func()) { out.field(label); value(); out.byte('\n') }

func format(data cpuData, option options, target []byte) []byte {
	out := output{data: target}
	switch {
	case option.listColumns:
		out.text("CPU columns: CPU ONLINE SOCKET CORE THREAD APICID ACPIUID\n")
		out.text("Cache columns: NAME ONE-SIZE ALL-SIZE TYPE LEVEL WAYS SETS COHERENCY-SIZE SHARED-CPUS\n")
	case option.json:
		formatJSON(&out, data, option)
	case option.mode == modeExtended:
		formatCPUTable(&out, data, option, false)
	case option.mode == modeParse:
		formatCPUTable(&out, data, option, true)
	case option.mode == modeCaches:
		formatCacheTable(&out, data, option)
	default:
		formatSummary(&out, data, option)
	}
	return out.data
}

func formatSummary(out *output, data cpuData, option options) {
	summary := data.summary
	out.line("Architecture:", func() { out.text("x86_64") })
	out.line("CPU op-mode(s):", func() {
		if summary.OperatingModes&cpuinfo.Mode32 != 0 {
			out.text("32-bit, ")
		}
		out.text("64-bit")
	})
	if summary.Validity&cpuinfo.ValidAddressWidths != 0 {
		out.line("Address sizes:", func() {
			out.uint(uint64(summary.PhysicalAddressBits))
			out.text(" bits physical, ")
			out.uint(uint64(summary.LinearAddressBits))
			out.text(" bits virtual")
		})
	}
	out.line("Byte Order:", func() { out.text("Little Endian") })
	out.line("CPU(s):", func() { out.uint(uint64(summary.PresentLogicalCPUs)) })
	if option.hex {
		out.line("On-line CPU(s) mask:", func() { appendMask(out, data, true) })
		out.line("Off-line CPU(s) mask:", func() { appendMask(out, data, false) })
	} else {
		out.line("On-line CPU(s) list:", func() { appendCPUList(out, data, true) })
		if summary.PresentLogicalCPUs > summary.OnlineCPUs {
			out.line("Off-line CPU(s) list:", func() { appendCPUList(out, data, false) })
		}
	}
	if summary.Validity&cpuinfo.ValidIdentity != 0 {
		out.line("Vendor ID:", func() { appendSafeText(out, summary.Vendor[:]) })
		if summary.Brand[0] != 0 {
			out.line("Model name:", func() { appendSafeText(out, summary.Brand[:]) })
		}
		out.line("CPU family:", func() { out.uint(uint64(summary.Family)) })
		out.line("Model:", func() { out.uint(uint64(summary.Model)) })
		out.line("Stepping:", func() { out.uint(uint64(summary.Stepping)) })
	}
	if summary.Validity&cpuinfo.ValidTopology != 0 {
		out.line("Thread(s) per core:", func() { out.uint(uint64(summary.ThreadsPerCore)) })
		out.line("Core(s) per socket:", func() { out.uint(uint64(summary.CoresPerSocket)) })
		out.line("Socket(s):", func() { out.uint(uint64(summary.Sockets)) })
	}
	if summary.Validity&cpuinfo.ValidFrequency != 0 && summary.MaximumMHz != 0 {
		out.line("CPU max MHz:", func() { out.uint(uint64(summary.MaximumMHz)); out.text(".0000") })
	}
	if summary.HardwareFeatures.Has(cpuinfo.FeatureVMX) {
		out.line("Virtualization:", func() { out.text("VT-x") })
	} else if summary.HardwareFeatures.Has(cpuinfo.FeatureSVM) {
		out.line("Virtualization:", func() { out.text("AMD-V") })
	}
	if summary.Validity&cpuinfo.ValidHypervisor != 0 {
		out.line("Hypervisor vendor:", func() { appendHypervisor(out, summary.HypervisorVendor[:]) })
		out.line("Virtualization type:", func() { out.text("full") })
	}
	appendCacheSummary(out, data, option)
	out.line("Flags:", func() { appendFeatures(out, summary.UsableFeatures) })
}

func formatJSON(out *output, data cpuData, option options) {
	out.text("{\"lscpu\":[")
	first := true
	jsonField := func(field string, value func()) {
		if !first {
			out.byte(',')
		}
		first = false
		out.text("{\"field\":\"")
		jsonString(out, []byte(field))
		out.text("\",\"data\":\"")
		value()
		out.text("\"}")
	}
	jsonField("Architecture:", func() { out.text("x86_64") })
	jsonField("CPU op-mode(s):", func() {
		if data.summary.OperatingModes&cpuinfo.Mode32 != 0 {
			out.text("32-bit, ")
		}
		out.text("64-bit")
	})
	if data.summary.Validity&cpuinfo.ValidAddressWidths != 0 {
		jsonField("Address sizes:", func() {
			out.uint(uint64(data.summary.PhysicalAddressBits))
			out.text(" bits physical, ")
			out.uint(uint64(data.summary.LinearAddressBits))
			out.text(" bits virtual")
		})
	}
	jsonField("Byte Order:", func() { out.text("Little Endian") })
	jsonField("CPU(s):", func() { out.uint(uint64(data.summary.PresentLogicalCPUs)) })
	setLabel := "On-line CPU(s) list:"
	if option.hex {
		setLabel = "On-line CPU(s) mask:"
	}
	jsonField(setLabel, func() {
		if option.hex {
			appendMask(out, data, true)
		} else {
			appendCPUList(out, data, true)
		}
	})
	if data.summary.PresentLogicalCPUs > data.summary.OnlineCPUs {
		setLabel = "Off-line CPU(s) list:"
		if option.hex {
			setLabel = "Off-line CPU(s) mask:"
		}
		jsonField(setLabel, func() {
			if option.hex {
				appendMask(out, data, false)
			} else {
				appendCPUList(out, data, false)
			}
		})
	}
	if data.summary.Vendor[0] != 0 {
		jsonField("Vendor ID:", func() { jsonSafeText(out, data.summary.Vendor[:]) })
	}
	if data.summary.Brand[0] != 0 {
		jsonField("Model name:", func() { jsonSafeText(out, data.summary.Brand[:]) })
	}
	if data.summary.Validity&cpuinfo.ValidIdentity != 0 {
		jsonField("CPU family:", func() { out.uint(uint64(data.summary.Family)) })
		jsonField("Model:", func() { out.uint(uint64(data.summary.Model)) })
		jsonField("Stepping:", func() { out.uint(uint64(data.summary.Stepping)) })
	}
	if data.summary.Validity&cpuinfo.ValidTopology != 0 {
		jsonField("Thread(s) per core:", func() { out.uint(uint64(data.summary.ThreadsPerCore)) })
		jsonField("Core(s) per socket:", func() { out.uint(uint64(data.summary.CoresPerSocket)) })
		jsonField("Socket(s):", func() { out.uint(uint64(data.summary.Sockets)) })
	}
	if data.summary.Validity&cpuinfo.ValidFrequency != 0 && data.summary.MaximumMHz != 0 {
		jsonField("CPU max MHz:", func() { out.uint(uint64(data.summary.MaximumMHz)); out.text(".0000") })
	}
	if data.summary.HardwareFeatures.Has(cpuinfo.FeatureVMX) {
		jsonField("Virtualization:", func() { out.text("VT-x") })
	} else if data.summary.HardwareFeatures.Has(cpuinfo.FeatureSVM) {
		jsonField("Virtualization:", func() { out.text("AMD-V") })
	}
	if data.summary.Validity&cpuinfo.ValidHypervisor != 0 {
		jsonField("Hypervisor vendor:", func() {
			text := zeroText(data.summary.HypervisorVendor[:])
			switch string(text) {
			case "KVMKVMKVM":
				out.text("KVM")
			case "Microsoft Hv":
				out.text("Microsoft")
			case "VMwareVMware":
				out.text("VMware")
			case "XenVMMXenVMM":
				out.text("Xen")
			default:
				jsonString(out, text)
			}
		})
		jsonField("Virtualization type:", func() { out.text("full") })
	}
	for _, wanted := range []struct {
		level uint8
		kind  cpuinfo.CacheType
		label string
	}{{1, cpuinfo.CacheData, "L1d cache:"}, {1, cpuinfo.CacheInstruction, "L1i cache:"}, {2, cpuinfo.CacheUnified, "L2 cache:"}, {3, cpuinfo.CacheUnified, "L3 cache:"}} {
		for index := 0; index < data.cacheCount; index++ {
			cache := data.caches[index]
			if cache.Level == wanted.level && cache.Type == wanted.kind {
				size := cache.Size
				if total, ok := aggregateCache(data.summary, cache); ok {
					size = total
				}
				jsonField(wanted.label, func() { appendSize(out, size, option.bytes) })
				break
			}
		}
	}
	jsonField("Flags:", func() { appendFeatures(out, data.summary.UsableFeatures) })
	out.text("]}\n")
}

func appendFeatures(out *output, features cpuinfo.FeatureSet) {
	first := true
	for feature := cpuinfo.Feature(0); feature < cpuinfo.FeatureCount; feature++ {
		if !features.Has(feature) {
			continue
		}
		if !first {
			out.byte(' ')
		}
		first = false
		out.text(cpuinfo.FeatureName(feature))
	}
}

func appendSafeText(out *output, value []byte) {
	for _, character := range value {
		if character == 0 {
			break
		}
		if character < ' ' || character > '~' {
			character = '?'
		}
		out.byte(character)
	}
}

func jsonSafeText(out *output, value []byte) {
	end := 0
	for end < len(value) && value[end] != 0 {
		end++
	}
	jsonString(out, value[:end])
}

func jsonString(out *output, value []byte) {
	const hex = "0123456789abcdef"
	for _, character := range value {
		switch character {
		case '\\', '"':
			out.byte('\\')
			out.byte(character)
		case '\b':
			out.text("\\b")
		case '\f':
			out.text("\\f")
		case '\n':
			out.text("\\n")
		case '\r':
			out.text("\\r")
		case '\t':
			out.text("\\t")
		default:
			if character < 0x20 {
				out.text("\\u00")
				out.byte(hex[character>>4])
				out.byte(hex[character&15])
			} else {
				out.byte(character)
			}
		}
	}
}

func appendHypervisor(out *output, value []byte) {
	text := zeroText(value)
	switch string(text) {
	case "KVMKVMKVM":
		out.text("KVM")
	case "Microsoft Hv":
		out.text("Microsoft")
	case "VMwareVMware":
		out.text("VMware")
	case "XenVMMXenVMM":
		out.text("Xen")
	default:
		appendSafeText(out, text)
	}
}

func zeroText(value []byte) []byte {
	end := 0
	for end < len(value) && value[end] != 0 {
		end++
	}
	return value[:end]
}

func appendCPUList(out *output, data cpuData, online bool) {
	first := true
	for index := 0; index < data.entryCount; {
		matches := data.entries[index].Flags&cpuinfo.CPUEntryOnline != 0 == online
		if !matches {
			index++
			continue
		}
		start, end := index, index
		for end+1 < data.entryCount && (data.entries[end+1].Flags&cpuinfo.CPUEntryOnline != 0) == online {
			end++
		}
		if !first {
			out.byte(',')
		}
		first = false
		out.uint(uint64(data.entries[start].LogicalID))
		if end != start {
			out.byte('-')
			out.uint(uint64(data.entries[end].LogicalID))
		}
		index = end + 1
	}
}

func appendMask(out *output, data cpuData, online bool) {
	var words [4]uint64
	for index := 0; index < data.entryCount; index++ {
		if (data.entries[index].Flags&cpuinfo.CPUEntryOnline != 0) == online {
			words[data.entries[index].LogicalID/64] |= uint64(1) << (data.entries[index].LogicalID % 64)
		}
	}
	high := len(words) - 1
	for high > 0 && words[high] == 0 {
		high--
	}
	out.hex(words[high])
	for high--; high >= 0; high-- {
		out.byte(',')
		appendHex16(out, words[high])
	}
}

func appendHex16(out *output, value uint64) {
	const digits = "0123456789abcdef"
	for shift := 60; shift >= 0; shift -= 4 {
		out.byte(digits[byte(value>>uint(shift)&15)])
	}
}

func appendCacheSummary(out *output, data cpuData, option options) {
	for _, wanted := range []struct {
		level uint8
		kind  cpuinfo.CacheType
		label string
	}{{1, cpuinfo.CacheData, "L1d cache:"}, {1, cpuinfo.CacheInstruction, "L1i cache:"}, {2, cpuinfo.CacheUnified, "L2 cache:"}, {3, cpuinfo.CacheUnified, "L3 cache:"}} {
		for index := 0; index < data.cacheCount; index++ {
			cache := data.caches[index]
			if cache.Level != wanted.level || cache.Type != wanted.kind {
				continue
			}
			size := cache.Size
			if total, ok := aggregateCache(data.summary, cache); ok {
				size = total
			}
			out.line(wanted.label, func() { appendSize(out, size, option.bytes) })
			break
		}
	}
}

func appendSize(out *output, value uint64, bytes bool) {
	if bytes {
		out.uint(value)
		return
	}
	units := []string{"B", "KiB", "MiB", "GiB"}
	unit := 0
	for unit+1 < len(units) && value%1024 == 0 && value >= 1024 {
		value /= 1024
		unit++
	}
	out.uint(value)
	out.byte(' ')
	out.text(units[unit])
}

func selectedEntry(entry cpuinfo.CPUEntry, filter int) bool {
	online := entry.Flags&cpuinfo.CPUEntryOnline != 0
	return filter == filterAll || filter == filterOnline && online || filter == filterOffline && !online
}

func formatCPUTable(out *output, data cpuData, option options, parse bool) {
	columns := option.cpuColumns
	if parse {
		out.byte('#')
		appendCPUHeader(out, columns, ',', option.raw)
		out.byte('\n')
	} else {
		appendCPUHeader(out, columns, ' ', option.raw)
		out.byte('\n')
	}
	for index := 0; index < data.entryCount; index++ {
		entry := data.entries[index]
		if !selectedEntry(entry, option.filter) {
			continue
		}
		separator := byte(' ')
		if parse {
			separator = ','
		}
		first := true
		appendColumn := func(bit uint32, value func()) {
			if columns&bit == 0 {
				return
			}
			if !first {
				if parse {
					out.byte(separator)
				} else if option.raw {
					out.byte(separator)
				} else {
					out.text("  ")
				}
			}
			first = false
			value()
		}
		appendColumn(columnCPU, func() {
			if option.physical {
				out.uint(uint64(entry.APICID))
			} else {
				out.uint(uint64(entry.LogicalID))
			}
		})
		appendColumn(columnOnline, func() {
			if entry.Flags&cpuinfo.CPUEntryOnline != 0 {
				out.text("yes")
			} else {
				out.text("no")
			}
		})
		appendColumn(columnSocket, func() { appendTopologyCell(out, entry, entry.PackageID) })
		appendColumn(columnCore, func() { appendTopologyCell(out, entry, entry.CoreID) })
		appendColumn(columnThread, func() { appendTopologyCell(out, entry, entry.ThreadID) })
		appendColumn(columnAPICID, func() { out.uint(uint64(entry.APICID)) })
		appendColumn(columnACPIUID, func() {
			if entry.Flags&cpuinfo.CPUEntryACPIUIDValid != 0 {
				out.uint(uint64(entry.ACPIUID))
			} else {
				out.byte('-')
			}
		})
		out.byte('\n')
	}
}

func appendCPUHeader(out *output, columns uint32, separator byte, raw bool) {
	first := true
	for _, column := range []struct {
		bit  uint32
		name string
	}{{columnCPU, "CPU"}, {columnOnline, "ONLINE"}, {columnSocket, "SOCKET"}, {columnCore, "CORE"}, {columnThread, "THREAD"}, {columnAPICID, "APICID"}, {columnACPIUID, "ACPIUID"}} {
		if columns&column.bit == 0 {
			continue
		}
		if !first {
			if separator == ',' {
				out.byte(separator)
			} else if raw {
				out.byte(separator)
			} else {
				out.text("  ")
			}
		}
		first = false
		out.text(column.name)
	}
}
func appendTopologyCell(out *output, entry cpuinfo.CPUEntry, value uint32) {
	if entry.Flags&cpuinfo.CPUEntryTopologyValid == 0 {
		out.byte('-')
	} else {
		out.uint(uint64(value))
	}
}

func formatCacheTable(out *output, data cpuData, option options) {
	appendCacheHeader(out, option.cacheColumns, option.raw)
	out.byte('\n')
	for index := 0; index < data.cacheCount; index++ {
		cache := data.caches[index]
		first := true
		add := func(bit uint32, value func()) {
			if option.cacheColumns&bit == 0 {
				return
			}
			if !first {
				if option.raw {
					out.byte(' ')
				} else {
					out.text("  ")
				}
			}
			first = false
			value()
		}
		add(cacheName, func() {
			out.byte('L')
			out.uint(uint64(cache.Level))
			if cache.Type == cpuinfo.CacheData {
				out.byte('d')
			} else if cache.Type == cpuinfo.CacheInstruction {
				out.byte('i')
			}
		})
		add(cacheOneSize, func() { appendSize(out, cache.Size, option.bytes) })
		add(cacheAllSize, func() {
			if total, ok := aggregateCache(data.summary, cache); ok {
				appendSize(out, total, option.bytes)
			} else {
				out.byte('-')
			}
		})
		add(cacheType, func() {
			switch cache.Type {
			case cpuinfo.CacheData:
				out.text("Data")
			case cpuinfo.CacheInstruction:
				out.text("Instruction")
			default:
				out.text("Unified")
			}
		})
		add(cacheLevel, func() { out.uint(uint64(cache.Level)) })
		add(cacheWays, func() { out.uint(uint64(cache.Ways)) })
		add(cacheSets, func() { out.uint(uint64(cache.Sets)) })
		add(cacheCoherency, func() { out.uint(uint64(cache.LineSize)) })
		add(cacheSharedCPUs, func() { out.byte('-') })
		out.byte('\n')
	}
}
func appendCacheHeader(out *output, columns uint32, raw bool) {
	first := true
	for _, column := range []struct {
		bit  uint32
		name string
	}{{cacheName, "NAME"}, {cacheOneSize, "ONE-SIZE"}, {cacheAllSize, "ALL-SIZE"}, {cacheType, "TYPE"}, {cacheLevel, "LEVEL"}, {cacheWays, "WAYS"}, {cacheSets, "SETS"}, {cacheCoherency, "COHERENCY-SIZE"}, {cacheSharedCPUs, "SHARED-CPUS"}} {
		if columns&column.bit == 0 {
			continue
		}
		if !first {
			if raw {
				out.byte(' ')
			} else {
				out.text("  ")
			}
		}
		first = false
		out.text(column.name)
	}
}
func aggregateCache(summary cpuinfo.Summary, cache cpuinfo.CacheInfo) (uint64, bool) {
	if summary.Validity&cpuinfo.ValidTopology == 0 || cache.SharingCount == 0 || summary.PresentLogicalCPUs%cache.SharingCount != 0 {
		return 0, false
	}
	instances := uint64(summary.PresentLogicalCPUs / cache.SharingCount)
	if instances != 0 && cache.Size > ^uint64(0)/instances {
		return 0, false
	}
	return cache.Size * instances, true
}
