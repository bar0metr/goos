package main

import (
	"fmt"
	"os"
	"strconv"
)

const markerSize = 16

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: ppmcheck IMAGE.ppm")
		os.Exit(2)
	}
	data, err := os.ReadFile(os.Args[1])
	if err == nil {
		err = validate(data)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "framebuffer validation:", err)
		os.Exit(1)
	}
}

func validate(data []byte) error {
	magic, offset, err := token(data, 0)
	if err != nil || magic != "P6" {
		return fmt.Errorf("invalid PPM magic %q", magic)
	}
	widthText, offset, err := token(data, offset)
	if err != nil {
		return err
	}
	heightText, offset, err := token(data, offset)
	if err != nil {
		return err
	}
	maximumText, pixels, err := token(data, offset)
	if err != nil {
		return err
	}
	width, widthErr := strconv.Atoi(widthText)
	height, heightErr := strconv.Atoi(heightText)
	maximum, maximumErr := strconv.Atoi(maximumText)
	if widthErr != nil || heightErr != nil || maximumErr != nil || width < markerSize || height < markerSize ||
		width > 8192 || height > 8192 || maximum != 255 {
		return fmt.Errorf("unsupported PPM geometry %qx%q max %q", widthText, heightText, maximumText)
	}
	pixelBytes := width * height * 3
	if pixelBytes > len(data)-pixels {
		return fmt.Errorf("short pixel data for %dx%d image", width, height)
	}
	x0, y0 := width-markerSize, height-markerSize
	for y := 0; y < markerSize; y++ {
		for x := 0; x < markerSize; x++ {
			index := pixels + ((y0+y)*width+x0+x)*3
			expected := [3]byte{255, 0, 255}
			if (x+y)&1 != 0 {
				expected = [3]byte{0, 255, 255}
			}
			observed := [3]byte{data[index], data[index+1], data[index+2]}
			if observed != expected {
				magenta, cyan, minX, minY, maxX, maxY := locateMarkerColors(data[pixels:pixels+pixelBytes], width, height)
				return fmt.Errorf("pixel (%d,%d) = %v, want %v; image has magenta=%d cyan=%d bounds=(%d,%d)-(%d,%d)",
					x0+x, y0+y, observed, expected, magenta, cyan, minX, minY, maxX, maxY)
			}
		}
	}
	return nil
}

func locateMarkerColors(data []byte, width, height int) (magenta, cyan, minX, minY, maxX, maxY int) {
	minX, minY = width, height
	maxX, maxY = -1, -1
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			index := (y*width + x) * 3
			pixel := [3]byte{data[index], data[index+1], data[index+2]}
			if pixel == [3]byte{255, 0, 255} {
				magenta++
			} else if pixel == [3]byte{0, 255, 255} {
				cyan++
			} else {
				continue
			}
			if x < minX {
				minX = x
			}
			if x > maxX {
				maxX = x
			}
			if y < minY {
				minY = y
			}
			if y > maxY {
				maxY = y
			}
		}
	}
	return
}

func token(data []byte, offset int) (string, int, error) {
	for {
		for offset < len(data) && whitespace(data[offset]) {
			offset++
		}
		if offset >= len(data) {
			return "", offset, fmt.Errorf("truncated PPM header")
		}
		if data[offset] != '#' {
			break
		}
		for offset < len(data) && data[offset] != '\n' {
			offset++
		}
	}
	start := offset
	for offset < len(data) && !whitespace(data[offset]) {
		offset++
	}
	if start == offset || offset >= len(data) {
		return "", offset, fmt.Errorf("truncated PPM token")
	}
	value := string(data[start:offset])
	if data[offset] == '\r' && offset+1 < len(data) && data[offset+1] == '\n' {
		offset += 2
	} else {
		offset++
	}
	return value, offset, nil
}

func whitespace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\n' || value == '\r' || value == '\v' || value == '\f'
}
