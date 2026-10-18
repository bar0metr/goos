package main

import (
	"fmt"
	"strconv"
	"strings"
)

const pageBytes = 4096

// Table is a size-class table: slot sizes, ascending, the last 2048.
type Table struct {
	Name  string
	Sizes []int
}

// candidateTables are the tables the report compares. "dense" takes, for
// each slot count the current table uses, the largest multiple of 16 that
// still fits that many slots in a page; the others add classes to it or to
// the current table.
var candidateTables = []Table{
	{"current", []int{16, 32, 48, 64, 96, 128, 192, 256, 384, 512, 768, 1024, 2048}},
	{"dense", []int{16, 32, 48, 64, 96, 128, 192, 256, 400, 512, 816, 1024, 2048}},
	{"dense+1360", []int{16, 32, 48, 64, 96, 128, 192, 256, 400, 512, 816, 1024, 1360, 2048}},
	{"dense+320+672+1360", []int{16, 32, 48, 64, 96, 128, 192, 256, 320, 400, 512, 672, 816, 1024, 1360, 2048}},
	{"current+1280+1536+1792", []int{16, 32, 48, 64, 96, 128, 192, 256, 384, 512, 768, 1024, 1280, 1536, 1792, 2048}},
}

func parseTable(name, text string) (Table, error) {
	t := Table{Name: name}
	for _, field := range strings.Split(text, ",") {
		size, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil || size <= 0 || size%16 != 0 || size > 2048 {
			return t, fmt.Errorf("table %s: bad size %q", name, field)
		}
		if len(t.Sizes) > 0 && size <= t.Sizes[len(t.Sizes)-1] {
			return t, fmt.Errorf("table %s: sizes must ascend", name)
		}
		t.Sizes = append(t.Sizes, size)
	}
	if len(t.Sizes) == 0 || t.Sizes[len(t.Sizes)-1] != 2048 {
		return t, fmt.Errorf("table %s: the largest class must be 2048", name)
	}
	return t, nil
}

// classOf returns the slot size a request of up to limit bytes takes.
func (t Table) classOf(limit int) int {
	for _, size := range t.Sizes {
		if limit <= size {
			return size
		}
	}
	panic("request larger than the table")
}

// Fit is how a histogram of small requests fits a table: the slot bytes
// they take and the slab pages those slots fill. A live heap with the same
// mix of sizes fills its slabs the same way, so Utilization is what such a
// heap's full slabs hold; partly full slabs come on top.
type Fit struct {
	Requests, Requested, Slots uint64
	// Pages is the slab pages the slots fill: a slot of a class with n slots
	// per page costs 1/n of a page.
	Pages float64
}

// Internal is the share of slot bytes the requests did not ask for.
func (f Fit) Internal() float64 { return 1 - ratio(f.Requested, f.Slots) }

// Utilization is the share of the slab pages' bytes the requests asked for.
func (f Fit) Utilization() float64 {
	if f.Pages == 0 {
		return 0
	}
	return float64(f.Requested) / (f.Pages * pageBytes)
}

func (t Table) fit(sizes map[int][2]uint64) Fit {
	var f Fit
	perClass := map[int]uint64{}
	for bucket, entry := range sizes {
		class := t.classOf(16 * (bucket + 1))
		f.Requests += entry[0]
		f.Requested += entry[1]
		f.Slots += entry[0] * uint64(class)
		perClass[class] += entry[0]
	}
	for class, count := range perClass {
		f.Pages += float64(count) / float64(pageBytes/class)
	}
	return f
}

func ratio(a, b uint64) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}
