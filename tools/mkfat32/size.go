package main

import (
	"fmt"
	"os"
	"strings"
)

const mib = uint64(1 << 20)

// autoSizeMiB leaves at least 5% of the installed data's cluster demand free.
// Directory entry counts use UTF-8 byte lengths as an upper bound for UTF-16
// name units and round each directory up to full clusters.
func autoSizeMiB(puts []string, sector, perCluster, fats uint, scheme string) (uint64, error) {
	if sector == 0 {
		sector = 512
	}
	if sector < 512 || sector > 4096 || sector&(sector-1) != 0 ||
		perCluster == 0 || perCluster > 128 || perCluster&(perCluster-1) != 0 || fats == 0 || fats > 2 {
		return 0, fmt.Errorf("invalid FAT32 geometry for automatic size")
	}
	clusterBytes := uint64(sector) * uint64(perCluster)
	directories := map[string]uint64{"/": 0}
	var fileClusters uint64
	for _, put := range puts {
		separator := strings.IndexByte(put, ':')
		if separator < 0 {
			return 0, fmt.Errorf("invalid put %q", put)
		}
		source, target := put[:separator], strings.Trim(put[separator+1:], "/")
		if target == "" {
			return 0, fmt.Errorf("invalid target in %q", put)
		}
		parts := strings.Split(target, "/")
		parent := "/"
		for index, part := range parts {
			if part == "" {
				return 0, fmt.Errorf("invalid target in %q", put)
			}
			name := parent + part
			isDirectory := index < len(parts)-1 || source == ""
			if isDirectory {
				if _, exists := directories[name+"/"]; !exists {
					directories[parent] += uint64(1 + (len(part)+12)/13)
					directories[name+"/"] = 0
				}
				parent = name + "/"
				continue
			}
			directories[parent] += uint64(1 + (len(part)+12)/13)
		}
		if source == "" {
			continue
		}
		info, err := os.Stat(source)
		if err != nil {
			return 0, err
		}
		if !info.Mode().IsRegular() || info.Size() > int64(^uint32(0)) {
			return 0, fmt.Errorf("%s: FAT32 needs a regular file smaller than 4 GiB", source)
		}
		fileClusters += ceilDiv(uint64(info.Size()), clusterBytes)
	}
	payloadClusters := fileClusters
	for _, slots := range directories {
		payloadClusters += max(uint64(1), ceilDiv((slots+3)*32, clusterBytes))
	}
	if payloadClusters > ^uint64(0)/clusterBytes-1 {
		return 0, fmt.Errorf("tree is too large for automatic size")
	}
	wantedClusters := payloadClusters + ceilDiv(payloadClusters, 20)
	size := uint64(512)
	for {
		available, err := dataClusters(size, sector, perCluster, fats, scheme)
		if err != nil {
			return 0, err
		}
		if available >= wantedClusters {
			return size, nil
		}
		size += ceilDiv((wantedClusters-available)*clusterBytes, mib) + 1
	}
}

func dataClusters(sizeMiB uint64, sector, perCluster, fats uint, scheme string) (uint64, error) {
	if sizeMiB > (^uint64(0) >> 20) {
		return 0, fmt.Errorf("automatic disk size overflows")
	}
	bytes := sizeMiB * mib
	if scheme != "none" {
		if bytes <= 2*mib {
			return 0, fmt.Errorf("partitioned disk is too small")
		}
		bytes -= 2 * mib
	}
	total := bytes / uint64(sector)
	if total > uint64(^uint32(0)) {
		return 0, fmt.Errorf("automatic disk size exceeds FAT32 sector limit")
	}
	fatSectors := uint64(1)
	for {
		metadata := uint64(32) + uint64(fats)*fatSectors
		if total <= metadata+uint64(perCluster) {
			return 0, fmt.Errorf("disk is too small for FAT32")
		}
		clusters := (total - metadata) / uint64(perCluster)
		needed := ceilDiv((clusters+2)*4, uint64(sector))
		if needed <= fatSectors {
			if clusters > 0x0ffffff0-2 {
				return 0, fmt.Errorf("automatic disk size exceeds FAT32 cluster limit")
			}
			return clusters, nil
		}
		fatSectors = needed
	}
}

func ceilDiv(number, denominator uint64) uint64 {
	quotient := number / denominator
	if number%denominator != 0 {
		quotient++
	}
	return quotient
}
