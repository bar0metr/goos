// Package imagefixture stores disk images for tests compactly. Deflate
// compresses long runs of zeros only about a thousandfold, so an image is
// stored sparse first: gzip of "GOOSFIX1", the image size, then each 4 KiB
// block that is not all zeros as its offset and its bytes. It is used by
// tests and never linked into the system.
package imagefixture

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"io"
	"os"
)

const (
	magic = "GOOSFIX1"
	block = 4096
)

// Pack returns the stored form of an image.
func Pack(image []byte) []byte {
	var packed bytes.Buffer
	writer, _ := gzip.NewWriterLevel(&packed, gzip.BestCompression)
	var header [16]byte
	copy(header[:8], magic)
	binary.LittleEndian.PutUint64(header[8:], uint64(len(image)))
	writer.Write(header[:])
	zero := make([]byte, block)
	for offset := 0; offset < len(image); offset += block {
		data := image[offset:min(offset+block, len(image))]
		if bytes.Equal(data, zero[:len(data)]) {
			continue
		}
		var at [8]byte
		binary.LittleEndian.PutUint64(at[:], uint64(offset))
		writer.Write(at[:])
		writer.Write(append(data, zero[len(data):]...))
	}
	writer.Close()
	return packed.Bytes()
}

// Unpack writes the image stored at path into file, which it sizes first, so
// that the image stays sparse on the host.
func Unpack(path string, file *os.File) error {
	source, err := os.Open(path)
	if err != nil {
		return err
	}
	defer source.Close()
	reader, err := gzip.NewReader(source)
	if err != nil {
		return err
	}
	var header [16]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return err
	}
	if string(header[:8]) != magic {
		return errors.New("not an image fixture")
	}
	size := int64(binary.LittleEndian.Uint64(header[8:]))
	if err := file.Truncate(size); err != nil {
		return err
	}
	record := make([]byte, 8+block)
	for {
		if _, err := io.ReadFull(reader, record); err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}
		offset := int64(binary.LittleEndian.Uint64(record[:8]))
		if offset < 0 || offset >= size {
			return errors.New("fixture block outside the image")
		}
		if _, err := file.WriteAt(record[8:8+min(int64(block), size-offset)], offset); err != nil {
			return err
		}
	}
}
