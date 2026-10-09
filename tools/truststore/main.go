// truststore normalizes the locally pinned Mozilla-derived CA bundle.
package main

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const sourceSHA256 = "a41b5d356aea97a529fe27e0f7316d2f9d946d75927476cf9cf1b90637d00505"
const sourceDate = "2026-09-25T03:12:01Z"
const maxBundle = 512 << 10
const maxRoots = 256
const maxDER = 32 << 10

type metadata struct {
	SourceURL    string `json:"source_url"`
	ArchiveURL   string `json:"archive_url"`
	MozillaDate  string `json:"mozilla_date"`
	SourceSHA256 string `json:"source_sha256"`
	BundleSHA256 string `json:"bundle_sha256"`
	Certificates int    `json:"certificates"`
	BundleBytes  int    `json:"bundle_bytes"`
	License      string `json:"license"`
}

func main() {
	args := os.Args[1:]
	checkOnly := len(args) == 3 && args[0] == "-check"
	if checkOnly {
		args = args[1:]
	}
	if len(args) != 2 || args[0] == "-check" {
		fmt.Fprintln(os.Stderr, "usage: truststore [-check] LOCAL_SOURCE_PEM OUTPUT_PEM")
		os.Exit(2)
	}
	var err error
	if checkOnly {
		err = check(args[0], args[1], sourceSHA256)
	} else {
		err = generate(args[0], args[1], sourceSHA256)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "truststore:", err)
		os.Exit(1)
	}
}
func hash(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func normalize(data []byte, limit int) ([]byte, int, error) {
	if len(data) == 0 || len(data) > maxBundle {
		return nil, 0, errors.New("CA source size outside 1..512KiB")
	}
	begins := bytes.Count(data, []byte("-----BEGIN"))
	if begins == 0 || begins > limit || begins != bytes.Count(data, []byte("-----BEGIN CERTIFICATE-----")) || begins != bytes.Count(data, []byte("-----END CERTIFICATE-----")) || begins != bytes.Count(data, []byte("-----END")) {
		return nil, 0, errors.New("CA source has invalid PEM markers or root count")
	}
	output := make([]byte, 0, len(data))
	count := 0
	for {
		block, rest := pem.Decode(data)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" || len(block.Headers) != 0 || len(block.Bytes) == 0 || len(block.Bytes) > maxDER {
			return nil, 0, errors.New("CA source has invalid certificate PEM or DER size")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, 0, fmt.Errorf("CA certificate %d: %w", count+1, err)
		}
		if !cert.BasicConstraintsValid || !cert.IsCA || cert.KeyUsage != 0 && cert.KeyUsage&x509.KeyUsageCertSign == 0 {
			return nil, 0, errors.New("CA certificate lacks valid CA signing constraints")
		}
		count++
		output = append(output, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: block.Bytes})...)
		data = rest
	}
	if count != begins || len(output) > maxBundle {
		return nil, 0, errors.New("CA source contains undecodable PEM or oversized output")
	}
	return output, count, nil
}
func readLimited(path string, limit int64) ([]byte, error) {
	in, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(in, limit+1))
	closeErr := in.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s exceeds input size limit", path)
	}
	return data, nil
}

func prepare(source, expected string) ([]byte, []byte, error) {
	data, err := readLimited(source, maxBundle)
	if err != nil {
		return nil, nil, err
	}
	if hash(data) != expected {
		return nil, nil, errors.New("CA source SHA256 does not match the pinned bundle")
	}
	canonical, count, err := normalize(data, maxRoots)
	if err != nil {
		return nil, nil, err
	}
	record := metadata{SourceURL: "https://curl.se/ca/cacert.pem", ArchiveURL: "https://curl.se/ca/cacert-2026-09-25.pem", MozillaDate: sourceDate, SourceSHA256: hash(data), BundleSHA256: hash(canonical), Certificates: count, BundleBytes: len(canonical), License: "MPL-2.0"}
	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	return canonical, append(encoded, '\n'), nil
}

// check regenerates expected bytes in memory and never repairs either artifact.
func check(source, output, expected string) error {
	canonical, encoded, err := prepare(source, expected)
	if err != nil {
		return err
	}
	installed, err := readLimited(output, maxBundle)
	if err != nil {
		return err
	}
	if !bytes.Equal(installed, canonical) {
		return errors.New("CA artifact does not match the pinned source")
	}
	record, err := readLimited(output+".json", 64<<10)
	if err != nil {
		return err
	}
	if !bytes.Equal(record, encoded) {
		return errors.New("CA metadata does not match the pinned source")
	}
	return nil
}

func generate(source, output, expected string) error {
	canonical, encoded, err := prepare(source, expected)
	if err != nil {
		return err
	}
	sourceAbs, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	outputAbs, err := filepath.Abs(output)
	if err != nil {
		return err
	}
	sourceInfo, err := os.Stat(source)
	if err != nil {
		return err
	}
	for _, destination := range []string{outputAbs, outputAbs + ".json"} {
		if sourceAbs == destination {
			return errors.New("output must not replace the source bundle")
		}
		if outputInfo, err := os.Stat(destination); err == nil && os.SameFile(sourceInfo, outputInfo) {
			return errors.New("output aliases source bundle")
		}
	}
	if err := os.WriteFile(output, canonical, 0644); err != nil {
		return err
	}
	return os.WriteFile(output+".json", encoded, 0644)
}
