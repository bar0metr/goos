package main

const (
	modeSummary = iota
	modeExtended
	modeParse
	modeCaches
)

const (
	filterAll = iota
	filterOnline
	filterOffline
)

const (
	columnCPU uint32 = 1 << iota
	columnOnline
	columnSocket
	columnCore
	columnThread
	columnAPICID
	columnACPIUID
)

const (
	cacheName uint32 = 1 << iota
	cacheOneSize
	cacheAllSize
	cacheType
	cacheLevel
	cacheWays
	cacheSets
	cacheCoherency
	cacheSharedCPUs
)

const allCPUColumns = columnCPU | columnOnline | columnSocket | columnCore | columnThread | columnAPICID | columnACPIUID
const allCacheColumns = cacheName | cacheOneSize | cacheAllSize | cacheType | cacheLevel | cacheWays | cacheSets | cacheCoherency | cacheSharedCPUs

type options struct {
	mode         int
	filter       int
	cpuColumns   uint32
	cacheColumns uint32
	bytes        bool
	json         bool
	raw          bool
	hex          bool
	physical     bool
	listColumns  bool
	outputAll    bool
	help         bool
	version      bool
	tableSet     bool
	filterSet    bool
}

func defaultOptions() options {
	return options{filter: filterAll, cpuColumns: columnCPU | columnOnline | columnSocket | columnCore | columnThread | columnAPICID,
		cacheColumns: cacheName | cacheOneSize | cacheAllSize | cacheType | cacheLevel | cacheWays | cacheSets | cacheCoherency}
}

func (option *options) parse(argument []byte) string {
	switch string(argument) {
	case "-h", "--help":
		option.help = true
	case "-V", "--version":
		option.version = true
	case "-B", "--bytes":
		option.bytes = true
	case "-r", "--raw":
		option.raw = true
	case "-J", "--json":
		option.json = true
	case "-H", "--list-columns":
		option.listColumns = true
	case "-a", "--all":
		if option.filterSet {
			return "CPU filters are mutually exclusive"
		}
		option.filter, option.filterSet = filterAll, true
	case "-b", "--online":
		if option.filterSet {
			return "CPU filters are mutually exclusive"
		}
		option.filter, option.filterSet = filterOnline, true
	case "-c", "--offline":
		if option.filterSet {
			return "CPU filters are mutually exclusive"
		}
		option.filter, option.filterSet = filterOffline, true
	case "-x", "--hex":
		option.hex = true
	case "-y", "--physical":
		option.physical = true
	case "--output-all":
		option.outputAll = true
	case "-e", "--extended":
		if problem := option.setMode(modeExtended); problem != "" {
			return problem
		}
	case "-p", "--parse":
		if problem := option.setMode(modeParse); problem != "" {
			return problem
		}
	case "-C", "--caches":
		if problem := option.setMode(modeCaches); problem != "" {
			return problem
		}
	case "-s", "--sysroot", "--arm-id", "--arm-model", "--annotate", "--hierarchic":
		return "option is not supported by GoOS"
	default:
		name, value, found := splitOption(argument)
		if !found {
			return "unknown option"
		}
		switch string(name) {
		case "-e", "--extended":
			if problem := option.setMode(modeExtended); problem != "" {
				return problem
			}
			columns, ok := parseCPUColumns(value)
			if !ok {
				return "invalid extended column list"
			}
			option.cpuColumns = columns
		case "-p", "--parse":
			if problem := option.setMode(modeParse); problem != "" {
				return problem
			}
			columns, ok := parseCPUColumns(value)
			if !ok {
				return "invalid parse column list"
			}
			option.cpuColumns = columns
		case "-C", "--caches":
			if problem := option.setMode(modeCaches); problem != "" {
				return problem
			}
			columns, ok := parseCacheColumns(value)
			if !ok {
				return "invalid cache column list"
			}
			option.cacheColumns = columns
		default:
			return "unknown option"
		}
	}
	return ""
}

func (option *options) setMode(mode int) string {
	if option.tableSet && option.mode != mode {
		return "output modes are mutually exclusive"
	}
	option.mode, option.tableSet = mode, true
	return ""
}

func (option *options) validate() string {
	if option.filterSet && option.mode != modeExtended && option.mode != modeParse {
		return "CPU filter requires extended or parse mode"
	}
	if option.json && option.mode != modeSummary {
		return "JSON is supported only for summary output"
	}
	if option.raw && option.mode == modeSummary {
		return "raw output requires extended, parse, or caches mode"
	}
	if option.hex && option.mode != modeSummary {
		return "hex output is supported only for summary output"
	}
	if option.physical && option.mode != modeExtended && option.mode != modeParse {
		return "physical IDs require extended or parse mode"
	}
	if option.outputAll && !option.tableSet {
		return "output-all requires extended, parse, or caches mode"
	}
	if option.outputAll {
		option.cpuColumns, option.cacheColumns = allCPUColumns, allCacheColumns
	}
	return ""
}

func splitOption(argument []byte) ([]byte, []byte, bool) {
	for index, value := range argument {
		if value == '=' && index != 0 && index+1 < len(argument) {
			return argument[:index], argument[index+1:], true
		}
	}
	return nil, nil, false
}

func parseCPUColumns(value []byte) (uint32, bool) {
	return parseColumns(value, []columnName{{"CPU", columnCPU}, {"ONLINE", columnOnline}, {"SOCKET", columnSocket}, {"CORE", columnCore}, {"THREAD", columnThread}, {"APICID", columnAPICID}, {"ACPIUID", columnACPIUID}})
}

func parseCacheColumns(value []byte) (uint32, bool) {
	return parseColumns(value, []columnName{{"NAME", cacheName}, {"ONE-SIZE", cacheOneSize}, {"ALL-SIZE", cacheAllSize}, {"TYPE", cacheType}, {"LEVEL", cacheLevel}, {"WAYS", cacheWays}, {"SETS", cacheSets}, {"COHERENCY-SIZE", cacheCoherency}, {"SHARED-CPUS", cacheSharedCPUs}})
}

type columnName struct {
	name string
	bit  uint32
}

func parseColumns(value []byte, names []columnName) (uint32, bool) {
	var result uint32
	for len(value) != 0 {
		end := 0
		for end < len(value) && value[end] != ',' {
			end++
		}
		if end == 0 {
			return 0, false
		}
		found := false
		for _, candidate := range names {
			if equalFold(value[:end], candidate.name) {
				result |= candidate.bit
				found = true
				break
			}
		}
		if !found {
			return 0, false
		}
		if end == len(value) {
			break
		}
		value = value[end+1:]
	}
	return result, result != 0
}

func equalFold(value []byte, expected string) bool {
	if len(value) != len(expected) {
		return false
	}
	for index, character := range value {
		if character >= 'a' && character <= 'z' {
			character -= 'a' - 'A'
		}
		if character != expected[index] {
			return false
		}
	}
	return true
}
