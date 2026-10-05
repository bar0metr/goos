package fat32

import "goos/kernel/ramfs"

// Timestamps and attributes. The on-disk forms — DOS dates and times, the
// attribute byte — stay in this file; callers see nanoseconds since the Unix
// epoch and native attribute flags.
//
// Policy (docs/userspace/storage.md, ADR 0112):
//   - FAT records local time without a zone. GoOS has no time zone, so FAT
//     times are read and written as UTC.
//   - Times outside FAT's range, 1980-01-01 00:00:00 to 2107-12-31 23:59:59,
//     are clamped to it. Modification times have two-second resolution,
//     creation times ten milliseconds, access times one day.
//   - A field whose date is zero or invalid is unknown, and is preserved as
//     it is.
//   - Creating an entry records its creation, modification and access time.
//     Changing a file's data (write, truncate) records its modification and
//     access date and sets ARCHIVE. Creating, removing or renaming an entry
//     records the modification time of the directory holding it. Reading
//     records nothing: a read never writes metadata.
//   - With no wall clock every time a change would record is left as it
//     was, and a new entry's times are zero: unknown, not a made-up date.

// Clock reports the wall-clock time, in nanoseconds since the Unix epoch, and
// whether it is known.
type Clock func() (int64, bool)

// SetClock gives the filesystem the clock its timestamps are taken from. It
// survives Mount.
func (fs *FileSystem) SetClock(clock Clock) { fs.now = clock }

// Time is a timestamp read from a volume.
type Time struct {
	UnixNanos int64
	Known     bool
}

// Attributes are the FAT attributes a file or directory carries, in native
// form.
type Attributes uint8

const (
	// AttrReadOnly: the file's data cannot be changed; writes and
	// truncation are refused with permission denied. It may still be
	// renamed or removed, as in Linux. It means nothing on a directory.
	AttrReadOnly Attributes = 1 << iota
	AttrHidden
	AttrSystem
	// AttrArchive: the file changed since a backup tool last cleared it.
	AttrArchive
)

// Metadata is what a directory entry records besides name, kind and size.
type Metadata struct {
	Created    Time
	Modified   Time
	Accessed   Time
	Attributes Attributes
}

// On-disk attribute bits and the fields that hold times.
const (
	attrReadOnly  = byte(0x01)
	attrHidden    = byte(0x02)
	attrSystem    = byte(0x04)
	attrVolume    = byte(0x08)
	attrDirectory = byte(0x10)
	attrArchive   = byte(0x20)
	// caseLowerBase and caseLowerExt are the Windows NT flags in byte 12 of a
	// short entry that say its 8.3 name is displayed lower case.
	caseLowerBase = byte(0x08)
	caseLowerExt  = byte(0x10)

	fieldCreateTenths = 13
	fieldCreateTime   = 14
	fieldCreateDate   = 16
	fieldAccessDate   = 18
	fieldWriteTime    = 22
	fieldWriteDate    = 24
)

const (
	nanosPerSecond = int64(1000000000)
	secondsPerDay  = int64(86400)
	fatEpochYear   = 1980
	// fatFirst and fatLast are the first and last second FAT can record:
	// 1980-01-01 00:00:00 and 2107-12-31 23:59:59, as Unix seconds.
	fatFirst = int64(315532800)
	fatLast  = int64(4354819199)
)

// daysFromCivil is the number of days from 1970-01-01 to the given date in
// the proleptic Gregorian calendar.
func daysFromCivil(year, month, day int64) int64 {
	if month <= 2 {
		year--
	}
	era := year / 400
	yearOfEra := year - era*400
	shifted := month + 9
	if month > 2 {
		shifted = month - 3
	}
	dayOfYear := (153*shifted+2)/5 + day - 1
	dayOfEra := yearOfEra*365 + yearOfEra/4 - yearOfEra/100 + dayOfYear
	return era*146097 + dayOfEra - 719468
}

// civilFromDays inverts daysFromCivil for days not before 1970.
func civilFromDays(days int64) (int64, int64, int64) {
	days += 719468
	era := days / 146097
	dayOfEra := days - era*146097
	yearOfEra := (dayOfEra - dayOfEra/1460 + dayOfEra/36524 - dayOfEra/146096) / 365
	dayOfYear := dayOfEra - (365*yearOfEra + yearOfEra/4 - yearOfEra/100)
	shifted := (5*dayOfYear + 2) / 153
	day := dayOfYear - (153*shifted+2)/5 + 1
	month := shifted + 3
	if shifted >= 10 {
		month = shifted - 9
	}
	year := yearOfEra + era*400
	if month <= 2 {
		year++
	}
	return year, month, day
}

func daysInMonth(year, month int64) int64 {
	switch month {
	case 2:
		if year%4 == 0 && (year%100 != 0 || year%400 == 0) {
			return 29
		}
		return 28
	case 4, 6, 9, 11:
		return 30
	}
	return 31
}

// fatStamp is a time in FAT's encoding.
type fatStamp struct {
	date   uint16
	time   uint16
	tenths uint8
}

// encodeTime converts Unix nanoseconds to FAT's encoding, clamped to the
// range FAT can record.
func encodeTime(unixNanos int64) fatStamp {
	seconds, nanos := unixNanos/nanosPerSecond, unixNanos%nanosPerSecond
	if nanos < 0 {
		seconds, nanos = seconds-1, nanos+nanosPerSecond
	}
	switch {
	case seconds < fatFirst:
		seconds, nanos = fatFirst, 0
	case seconds > fatLast:
		seconds, nanos = fatLast, nanosPerSecond-1
	}
	year, month, day := civilFromDays(seconds / secondsPerDay)
	within := seconds % secondsPerDay
	hour, minute, second := within/3600, within/60%60, within%60
	return fatStamp{
		date:   uint16((year-fatEpochYear)<<9 | month<<5 | day),
		time:   uint16(hour<<11 | minute<<5 | second/2),
		tenths: uint8(second%2*100 + nanos/10000000),
	}
}

// decodeTime converts FAT's encoding to Unix nanoseconds. A zero or invalid
// date, or an invalid time, is unknown.
func decodeTime(date, clock uint16, tenths uint8) Time {
	year, month, day := int64(date>>9)+fatEpochYear, int64(date>>5&0x0f), int64(date&0x1f)
	hour, minute, halves := int64(clock>>11), int64(clock>>5&0x3f), int64(clock&0x1f)
	if month < 1 || month > 12 || day < 1 || day > daysInMonth(year, month) ||
		hour > 23 || minute > 59 || halves > 29 || tenths > 199 {
		return Time{}
	}
	seconds := daysFromCivil(year, month, day)*secondsPerDay + hour*3600 + minute*60 + halves*2 + int64(tenths/100)
	return Time{UnixNanos: seconds*nanosPerSecond + int64(tenths%100)*10000000, Known: true}
}

// metadataOf decodes the times and attributes of a short entry.
func metadataOf(entry [32]byte) Metadata {
	var metadata Metadata
	metadata.Created = decodeTime(get16(entry[fieldCreateDate:]), get16(entry[fieldCreateTime:]), entry[fieldCreateTenths])
	metadata.Modified = decodeTime(get16(entry[fieldWriteDate:]), get16(entry[fieldWriteTime:]), 0)
	metadata.Accessed = decodeTime(get16(entry[fieldAccessDate:]), 0, 0)
	attributes := entry[11]
	if attributes&attrReadOnly != 0 && attributes&attrDirectory == 0 {
		metadata.Attributes |= AttrReadOnly
	}
	if attributes&attrHidden != 0 {
		metadata.Attributes |= AttrHidden
	}
	if attributes&attrSystem != 0 {
		metadata.Attributes |= AttrSystem
	}
	if attributes&attrArchive != 0 {
		metadata.Attributes |= AttrArchive
	}
	return metadata
}

// Metadata reports the times and attributes of a node. The root directory has
// no entry and so none of either.
func (fs *FileSystem) Metadata(id ramfs.NodeID) (Metadata, ramfs.Status) {
	if id == rootNode && fs.mounted {
		return Metadata{}, ramfs.StatusOK
	}
	_, _, entry, status := fs.node(id)
	if status != ramfs.StatusOK {
		return Metadata{}, status
	}
	return metadataOf(entry), ramfs.StatusOK
}

// wallClock is the current time when the filesystem has a clock that knows it.
func (fs *FileSystem) wallClock() (fatStamp, bool) {
	if fs.now == nil {
		return fatStamp{}, false
	}
	now, ok := fs.now()
	if !ok {
		return fatStamp{}, false
	}
	return encodeTime(now), true
}

// stampCreated records creation, modification and access in a new entry, or
// leaves them zero when there is no clock.
func (fs *FileSystem) stampCreated(entry *[32]byte) {
	now, _ := fs.wallClock()
	entry[fieldCreateTenths] = now.tenths
	put16(entry[fieldCreateTime:], now.time)
	put16(entry[fieldCreateDate:], now.date)
	put16(entry[fieldAccessDate:], now.date)
	put16(entry[fieldWriteTime:], now.time)
	put16(entry[fieldWriteDate:], now.date)
}

// stampModified records a change of a file's data: ARCHIVE is set, and the
// modification time and access date when there is a clock. It reports whether
// the entry changed.
func (fs *FileSystem) stampModified(entry *[32]byte) bool {
	before := *entry
	entry[11] |= attrArchive
	if now, ok := fs.wallClock(); ok {
		put16(entry[fieldWriteTime:], now.time)
		put16(entry[fieldWriteDate:], now.date)
		put16(entry[fieldAccessDate:], now.date)
	}
	return *entry != before
}

// touchDirectory records that the directory dir changed: the modification
// time of its entry in its parent. The root has no entry, and without a clock
// there is nothing to record. The entry is found by its first cluster. It is
// best effort: the change it records has already been made, so a parent that
// cannot be read, or a damaged ".." that leads nowhere, leaves the old time
// rather than reporting a completed operation as failed.
func (fs *FileSystem) touchDirectory(dir *directory) {
	now, ok := fs.wallClock()
	if !ok || dir.key == 0 {
		return
	}
	parentKey, status := fs.parentKey(dir.key)
	if status != ramfs.StatusOK {
		return
	}
	parent, status := fs.openDirectory(parentKey)
	if status != ramfs.StatusOK {
		return
	}
	for index := 0; index < fs.entries(&parent); index++ {
		entry, status := fs.readEntry(&parent, index)
		if status != ramfs.StatusOK || entry[0] == entryEnd {
			return
		}
		if !listed(entry) || !isDirectory(entry) || entryCluster(entry) != dir.key {
			continue
		}
		put16(entry[fieldWriteTime:], now.time)
		put16(entry[fieldWriteDate:], now.date)
		put16(entry[fieldAccessDate:], now.date)
		fs.writeEntry(&parent, index, entry)
		return
	}
}

// SetMetadata writes a node's modification time into its directory entry
// and sets or clears a file's READ_ONLY attribute. FAT records the time to
// two seconds and keeps no other permission. The root directory has no
// entry and refuses.
func (fs *FileSystem) SetMetadata(id ramfs.NodeID, change ramfs.MetadataChange) ramfs.Status {
	if !change.Valid() {
		return ramfs.StatusInvalidArgument
	}
	if status := fs.mutable(); status != ramfs.StatusOK {
		return status
	}
	if id == rootNode {
		return ramfs.StatusPermissionDenied
	}
	dir, index, entry, status := fs.node(id)
	if status != ramfs.StatusOK {
		return status
	}
	if change.SetReadOnly && isDirectory(entry) {
		return ramfs.StatusIsDirectory
	}
	if change.SetModified {
		stamp := encodeTime(change.Modified.Seconds*nanosPerSecond + int64(change.Modified.Nanoseconds))
		put16(entry[fieldWriteTime:], stamp.time)
		put16(entry[fieldWriteDate:], stamp.date)
	}
	if change.SetReadOnly && change.ReadOnly {
		entry[11] |= attrReadOnly
	} else if change.SetReadOnly {
		entry[11] &^= attrReadOnly
	}
	if status = fs.writeEntry(&dir, index, entry); status != ramfs.StatusOK {
		return status
	}
	return fs.sync()
}

// mutable refuses a change to a volume mounted read-only.
func (fs *FileSystem) mutable() ramfs.Status {
	if fs.readOnly {
		return ramfs.StatusPermissionDenied
	}
	return ramfs.StatusOK
}

// writable refuses a change to the data of a read-only file.
func writable(entry [32]byte) ramfs.Status {
	if entry[11]&attrReadOnly != 0 {
		return ramfs.StatusPermissionDenied
	}
	return ramfs.StatusOK
}
