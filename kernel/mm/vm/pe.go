package vm

import "encoding/binary"

const (
	maxSections       = 16
	peSignature       = uint32(0x00004550)
	pe32PlusMagic     = uint16(0x20b)
	sectionExecutable = uint32(0x20000000)
	sectionWritable   = uint32(0x80000000)
)

type Section struct {
	Address     uint64
	Size        uint64
	Permissions Permissions
}

type Image struct {
	// Base is the address the image runs at.
	Base         uint64
	Size         uint64
	Sections     [maxSections]Section
	SectionCount int
}

// HeaderImageSize returns SizeOfImage when header starts with an MZ stub
// pointing at a PE32+ header inside it, and that size is nonzero and at most
// KernelImageMaxSize.
func HeaderImageSize(header []byte) (uint64, bool) {
	if len(header) < 0x40 || header[0] != 'M' || header[1] != 'Z' {
		return 0, false
	}
	peOffset := uint64(binary.LittleEndian.Uint32(header[0x3c:]))
	if peOffset > uint64(len(header)) || uint64(len(header))-peOffset < 24+60 {
		return 0, false
	}
	if binary.LittleEndian.Uint32(header[peOffset:]) != peSignature ||
		binary.LittleEndian.Uint16(header[peOffset+24:]) != pe32PlusMagic {
		return 0, false
	}
	size := uint64(binary.LittleEndian.Uint32(header[peOffset+24+56:]))
	if size == 0 || size > KernelImageMaxSize {
		return 0, false
	}
	return size, true
}

func ParsePE(data []byte, loadedBase uint64) (Image, bool) {
	var image Image
	if len(data) < 0x40 || data[0] != 'M' || data[1] != 'Z' {
		return image, false
	}
	peOffset := uint64(binary.LittleEndian.Uint32(data[0x3c:]))
	if peOffset > uint64(len(data))-24 || binary.LittleEndian.Uint32(data[peOffset:]) != peSignature {
		return image, false
	}
	numberOfSections := int(binary.LittleEndian.Uint16(data[peOffset+6:]))
	optionalSize := uint64(binary.LittleEndian.Uint16(data[peOffset+20:]))
	optionalOffset := peOffset + 24
	if numberOfSections == 0 || numberOfSections > maxSections || optionalSize < 60 || optionalOffset+optionalSize > uint64(len(data)) {
		return image, false
	}
	if binary.LittleEndian.Uint16(data[optionalOffset:]) != pe32PlusMagic {
		return image, false
	}
	image.Size = uint64(binary.LittleEndian.Uint32(data[optionalOffset+56:]))
	if image.Size == 0 || image.Size > uint64(len(data)) {
		return Image{}, false
	}
	sectionOffset := optionalOffset + optionalSize
	if sectionOffset+uint64(numberOfSections)*40 > uint64(len(data)) {
		return Image{}, false
	}
	for index := 0; index < numberOfSections; index++ {
		offset := sectionOffset + uint64(index)*40
		virtualSize := uint64(binary.LittleEndian.Uint32(data[offset+8:]))
		virtualAddress := uint64(binary.LittleEndian.Uint32(data[offset+12:]))
		characteristics := binary.LittleEndian.Uint32(data[offset+36:])
		if virtualSize == 0 || virtualAddress+virtualSize < virtualAddress || virtualAddress+virtualSize > image.Size {
			return Image{}, false
		}
		permissions := Permissions{
			Writable:   characteristics&sectionWritable != 0,
			Executable: characteristics&sectionExecutable != 0,
		}
		if !permissions.Valid() {
			return Image{}, false
		}
		image.Sections[index] = Section{
			Address:     loadedBase + virtualAddress,
			Size:        virtualSize,
			Permissions: permissions,
		}
	}
	image.SectionCount = numberOfSections
	image.Base = loadedBase
	return image, true
}
