package main

import (
	"archive/tar"
	"fmt"
	"os"
	"time"
)

func main() {
	if len(os.Args) != 6 {
		fmt.Fprintln(os.Stderr, "usage: initramfs INIT_IMAGE SERVICE_IMAGE VFS_IMAGE SHELL_IMAGE OUTPUT")
		os.Exit(2)
	}
	image, err := os.ReadFile(os.Args[1])
	if err != nil {
		fail(err)
	}
	service, err := os.ReadFile(os.Args[2])
	if err != nil {
		fail(err)
	}
	vfs, err := os.ReadFile(os.Args[3])
	if err != nil {
		fail(err)
	}
	shell, err := os.ReadFile(os.Args[4])
	if err != nil {
		fail(err)
	}
	file, err := os.Create(os.Args[5])
	if err != nil {
		fail(err)
	}
	writer := tar.NewWriter(file)
	epoch := time.Unix(0, 0)
	headers := []tar.Header{
		{Name: "system/", Mode: 0o755, Typeflag: tar.TypeDir, ModTime: epoch, Format: tar.FormatUSTAR},
		{Name: "system/init", Mode: 0o555, Size: int64(len(image)), Typeflag: tar.TypeReg, ModTime: epoch, Format: tar.FormatUSTAR},
		{Name: "system/fault-probe", Mode: 0o555, Size: int64(len(service)), Typeflag: tar.TypeReg, ModTime: epoch, Format: tar.FormatUSTAR},
		{Name: "system/vfs", Mode: 0o555, Size: int64(len(vfs)), Typeflag: tar.TypeReg, ModTime: epoch, Format: tar.FormatUSTAR},
		{Name: "system/shell", Mode: 0o555, Size: int64(len(shell)), Typeflag: tar.TypeReg, ModTime: epoch, Format: tar.FormatUSTAR},
	}
	for index := range headers {
		if err := writer.WriteHeader(&headers[index]); err != nil {
			fail(err)
		}
		if headers[index].Typeflag == tar.TypeReg {
			payload := image
			if headers[index].Name == "system/fault-probe" {
				payload = service
			} else if headers[index].Name == "system/vfs" {
				payload = vfs
			} else if headers[index].Name == "system/shell" {
				payload = shell
			}
			if _, err := writer.Write(payload); err != nil {
				fail(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		fail(err)
	}
	if err := file.Close(); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
