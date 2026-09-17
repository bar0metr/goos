package main

import (
	"fmt"
	"os"
)

const imageSize = int64(64 * 1024 * 1024)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: mkfat32 IMAGE")
		os.Exit(2)
	}
	path := os.Args[1]
	if valid(path) {
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
	if err != nil {
		fail(err)
	}
	defer file.Close()
	if err = file.Truncate(imageSize); err != nil {
		fail(err)
	}
	sector := make([]byte, 512)
	copy(sector[3:11], []byte("GOOSFAT "))
	put16(sector[11:13], 512)
	sector[13] = 1
	put16(sector[14:16], 32)
	sector[16] = 1
	put32(sector[32:36], uint32(imageSize/512))
	put32(sector[36:40], 1024)
	put32(sector[44:48], 2)
	put16(sector[48:50], 1)
	put16(sector[50:52], 6)
	copy(sector[82:90], []byte("FAT32   "))
	sector[510], sector[511] = 0x55, 0xaa
	if _, err = file.WriteAt(sector, 0); err != nil {
		fail(err)
	}
	for i := range sector {
		sector[i] = 0
	}
	put32(sector[0:4], 0x0ffffff8)
	put32(sector[4:8], 0xffffffff)
	put32(sector[8:12], 0x0fffffff)
	if _, err = file.WriteAt(sector, 32*512); err != nil {
		fail(err)
	}
}

func valid(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() != imageSize {
		return false
	}
	var data [512]byte
	if _, err = file.Read(data[:]); err != nil {
		return false
	}
	return data[510] == 0x55 && data[511] == 0xaa && string(data[82:90]) == "FAT32   "
}
func put16(data []byte, value uint16) { data[0] = byte(value); data[1] = byte(value >> 8) }
func put32(data []byte, value uint32) {
	for i := 0; i < 4; i++ {
		data[i] = byte(value >> uint(i*8))
	}
}
func fail(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
