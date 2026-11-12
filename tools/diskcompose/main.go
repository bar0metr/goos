// Command diskcompose builds a GPT disk image from partition images, for
// tests that need several volumes on one disk (ADR 0222):
//
//	diskcompose -part NAME:TYPE:SOURCE [-part ...] OUTPUT
//	diskcompose -extract NAME DISK OUTPUT
//
// TYPE is data (Microsoft basic data, what the persistent volume uses) or
// linux (Linux filesystem data). SOURCE is an image file, gzip-compressed
// when it ends in .gz, or zero:MiB for an empty partition. Partitions start
// on MiB boundaries in the order given. The output is written sparse.
//
// -extract copies the bytes of the GPT partition called NAME out of DISK, so
// a test can check a volume a guest wrote with host tools.
package main

import (
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"goos/userspace/partition"
)

const (
	blockSize = 512
	alignment = 1 << 20
)

type part struct {
	name   string
	kind   partition.GUID
	source string
	size   uint64
}

type fileWriter struct{ file *os.File }

func (writer fileWriter) WriteAt(offset uint64, data []byte) bool {
	_, err := writer.file.WriteAt(data, int64(offset))
	return err == nil
}

var typeLinux, _ = partition.ParseGUID("0FC63DAF-8483-4772-8E79-3D69D8477DE4")

func openSource(source string) (io.ReadCloser, uint64, error) {
	if size, found := strings.CutPrefix(source, "zero:"); found {
		mib, err := strconv.ParseUint(size, 10, 32)
		if err != nil || mib == 0 {
			return nil, 0, fmt.Errorf("bad size %q", size)
		}
		return nil, mib << 20, nil
	}
	file, err := os.Open(source)
	if err != nil {
		return nil, 0, err
	}
	if !strings.HasSuffix(source, ".gz") {
		info, err := file.Stat()
		if err != nil {
			file.Close()
			return nil, 0, err
		}
		return file, uint64(info.Size()), nil
	}
	reader, err := gzip.NewReader(file)
	if err != nil {
		file.Close()
		return nil, 0, err
	}
	size, err := io.Copy(io.Discard, reader)
	file.Close()
	if err != nil {
		return nil, 0, err
	}
	file, _ = os.Open(source)
	reader, _ = gzip.NewReader(file)
	return struct {
		io.Reader
		io.Closer
	}{reader, file}, uint64(size), nil
}

// copySparse copies reader to file at offset, skipping all-zero blocks.
func copySparse(file *os.File, offset uint64, reader io.Reader) error {
	buffer := make([]byte, 64<<10)
	for {
		count, err := io.ReadFull(reader, buffer)
		if count > 0 {
			chunk := buffer[:count]
			zero := true
			for _, value := range chunk {
				if value != 0 {
					zero = false
					break
				}
			}
			if !zero {
				if _, err := file.WriteAt(chunk, int64(offset)); err != nil {
					return err
				}
			}
			offset += uint64(count)
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// deterministicGUID derives a partition or disk GUID from a name, so the
// same arguments always build the same disk.
func deterministicGUID(name string) partition.GUID {
	sum := sha256.Sum256([]byte("goos diskcompose " + name))
	var guid partition.GUID
	copy(guid[:], sum[:16])
	guid[7] = guid[7]&0x0f | 0x40
	guid[8] = guid[8]&0x3f | 0x80
	return guid
}

// extract writes the partition called name in disk to output.
func extract(name, disk, output string) error {
	data, err := os.ReadFile(disk)
	if err != nil {
		return err
	}
	var scanner partition.Scanner
	var table partition.Table
	if status := scanner.Scan(imageReader{data}, uint64(len(data)), blockSize, &table); status != partition.StatusOK {
		return fmt.Errorf("partition table: status %d", status)
	}
	selector, ok := partition.NameSelector(name)
	if !ok {
		return fmt.Errorf("bad partition name %q", name)
	}
	chosen, status := partition.Select(&table, &selector)
	if status != partition.StatusOK {
		return fmt.Errorf("partition %q: status %d", name, status)
	}
	return os.WriteFile(output, data[chosen.Start:chosen.Start+chosen.Length], 0o644)
}

type imageReader struct{ data []byte }

func (reader imageReader) ReadAt(offset uint64, target []byte) bool {
	return offset+uint64(len(target)) <= uint64(len(reader.data)) && copy(target, reader.data[offset:]) == len(target)
}

func run() error {
	var parts []part
	extractName := flag.String("extract", "", "copy the partition NAME out of a disk: -extract NAME DISK OUTPUT")
	flag.Func("part", "add a partition NAME:TYPE:SOURCE (repeatable)", func(value string) error {
		fields := strings.SplitN(value, ":", 3)
		if len(fields) != 3 || fields[0] == "" {
			return errors.New("want NAME:TYPE:SOURCE")
		}
		entry := part{name: fields[0], source: fields[2]}
		switch fields[1] {
		case "data":
			entry.kind = partition.TypeBasicData
		case "linux":
			entry.kind = typeLinux
		default:
			return fmt.Errorf("unknown type %q", fields[1])
		}
		parts = append(parts, entry)
		return nil
	})
	flag.Parse()
	if *extractName != "" {
		if flag.NArg() != 2 || len(parts) != 0 {
			return errors.New("usage: diskcompose -extract NAME DISK OUTPUT")
		}
		return extract(*extractName, flag.Arg(0), flag.Arg(1))
	}
	if flag.NArg() != 1 || len(parts) == 0 {
		return errors.New("usage: diskcompose -part NAME:TYPE:SOURCE [-part ...] OUTPUT")
	}
	readers := make([]io.ReadCloser, len(parts))
	defer func() {
		for _, reader := range readers {
			if reader != nil {
				reader.Close()
			}
		}
	}()
	offset := uint64(alignment)
	var table []partition.GPTPartition
	for index := range parts {
		reader, size, err := openSource(parts[index].source)
		if err != nil {
			return fmt.Errorf("%s: %w", parts[index].name, err)
		}
		readers[index], parts[index].size = reader, size
		length := (size + alignment - 1) / alignment * alignment
		table = append(table, partition.GPTPartition{Type: parts[index].kind, Unique: deterministicGUID(parts[index].name),
			First: offset / blockSize, Last: (offset+length)/blockSize - 1, Name: parts[index].name})
		offset += length
	}
	capacity := offset + alignment
	file, err := os.OpenFile(flag.Arg(0), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Truncate(int64(capacity)); err != nil {
		return err
	}
	if !partition.WriteGPT(fileWriter{file}, capacity, blockSize, deterministicGUID("disk"), table) {
		return errors.New("partition table refused")
	}
	for index, reader := range readers {
		if reader == nil {
			continue
		}
		if err := copySparse(file, table[index].First*blockSize, reader); err != nil {
			return fmt.Errorf("%s: %w", parts[index].name, err)
		}
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "diskcompose:", err)
		os.Exit(1)
	}
}
