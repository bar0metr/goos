// truststore prepares locally supplied Mozilla certificate bundles.
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"goos/userspace/truststore"
)

const maxBundle = truststore.MaxBundle
const maxRoots = truststore.MaxRoots
const maxDER = 32 << 10

type options struct {
	Sequence                            uint64
	SourceDate, SourceURL, Old, SignKey string
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "truststore:", err)
		os.Exit(1)
	}
}

func run(args []string, report io.Writer) error {
	flags := flag.NewFlagSet("truststore", flag.ContinueOnError)
	var opts options
	checkOnly := flags.Bool("check", false, "verify source and artifacts without writing")
	keyDir := flags.String("keygen", "", "generate signing keys in an empty external directory")
	flags.Uint64Var(&opts.Sequence, "sequence", 0, "positive update sequence")
	flags.StringVar(&opts.SourceDate, "source-date", "", "UTC RFC3339 Mozilla source date")
	flags.StringVar(&opts.SourceURL, "source-url", "", "optional Mozilla source URL")
	flags.StringVar(&opts.Old, "old", "", "old bundle for root change report")
	flags.StringVar(&opts.SignKey, "sign-key", "", "raw Ed25519 private seed file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	maintenance := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name != "check" && f.Name != "keygen" {
			maintenance = true
		}
	})
	if *keyDir != "" {
		if *checkOnly || maintenance || flags.NArg() != 0 {
			return errors.New("keygen cannot be combined with other actions")
		}
		return keygen(*keyDir)
	}
	if flags.NArg() != 2 {
		return errors.New("usage: truststore [-check | -sequence N -source-date UTC_DATE] SOURCE OUTPUT.pem")
	}
	if *checkOnly {
		if maintenance {
			return errors.New("check cannot use generation flags")
		}
		return check(flags.Arg(0), flags.Arg(1))
	}
	return generate(flags.Arg(0), flags.Arg(1), opts, report)
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
		if !cert.BasicConstraintsValid || !cert.IsCA || cert.KeyUsage != 0 && cert.KeyUsage&x509.KeyUsageCertSign == 0 || len(cert.DNSNames) > 64 {
			return nil, 0, errors.New("CA certificate lacks valid CA signing constraints or exceeds DNS bounds")
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

func metadataPath(output string) string  { return strings.TrimSuffix(output, ".pem") + ".json" }
func signaturePath(output string) string { return strings.TrimSuffix(output, ".pem") + ".sig" }

func prepare(source string, opts options) ([]byte, []byte, error) {
	data, err := readLimited(source, maxBundle)
	if err != nil {
		return nil, nil, err
	}
	canonical, count, err := normalize(data, maxRoots)
	if err != nil {
		return nil, nil, err
	}
	if opts.SourceURL != "" {
		sourceURL, err := url.Parse(opts.SourceURL)
		if err != nil || sourceURL.Scheme != "https" || sourceURL.Host == "" || sourceURL.User != nil || sourceURL.Fragment != "" {
			return nil, nil, errors.New("source URL must be an HTTPS URL without credentials or fragment")
		}
	}
	meta := truststore.Metadata{Format: 1, Sequence: opts.Sequence, Source: "mozilla", SourceURL: opts.SourceURL, SourceDate: opts.SourceDate, SourceSHA256: hash(data), BundleSHA256: hash(canonical), Certificates: count, BundleBytes: len(canonical)}
	encoded, err := truststore.CanonicalMetadata(meta)
	return canonical, encoded, err
}

// check never repairs artifacts or downloads source material.
func check(source, output string) error {
	record, err := readLimited(metadataPath(output), truststore.MaxMetadata)
	if err != nil {
		return err
	}
	meta, err := truststore.ParseMetadata(record)
	if err != nil {
		return err
	}
	canonical, encoded, err := prepare(source, options{Sequence: meta.Sequence, SourceURL: meta.SourceURL, SourceDate: meta.SourceDate})
	if err != nil {
		return err
	}
	installed, err := readLimited(output, maxBundle)
	if err != nil {
		return err
	}
	if !bytes.Equal(canonical, installed) || !bytes.Equal(record, encoded) {
		return errors.New("CA artifacts do not match recorded source identity")
	}
	return nil
}

func guardOutputs(inputs, outputs []string) error {
	for i, destination := range outputs {
		destAbs, err := filepath.Abs(destination)
		if err != nil {
			return err
		}
		destInfo, statErr := os.Lstat(destination)
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
		if statErr == nil && !destInfo.Mode().IsRegular() {
			return errors.New("output must be a regular file, not a symlink")
		}
		for _, input := range append(append([]string{}, inputs...), outputs[:i]...) {
			inputAbs, err := filepath.Abs(input)
			if err != nil {
				return err
			}
			if destAbs == inputAbs {
				return errors.New("output aliases an input or another artifact")
			}
			info, err := os.Stat(input)
			if err == nil && destInfo != nil && os.SameFile(info, destInfo) {
				return errors.New("output aliases an input or another artifact")
			}
		}
	}
	return nil
}

func generate(source, output string, opts options, report io.Writer) error {
	if !strings.HasSuffix(output, ".pem") {
		return errors.New("output must end in .pem")
	}
	canonical, encoded, err := prepare(source, opts)
	if err != nil {
		return err
	}
	outputs := []string{output, metadataPath(output)}
	inputs := []string{source}
	var signature []byte
	if opts.SignKey != "" {
		inputs = append(inputs, opts.SignKey)
		seed, err := readLimited(opts.SignKey, ed25519.SeedSize)
		if err != nil {
			return err
		}
		if len(seed) != ed25519.SeedSize {
			return errors.New("signing seed must contain exactly 32 bytes")
		}
		message, err := truststore.SigningMessage(encoded, canonical)
		if err != nil {
			return err
		}
		signature = ed25519.Sign(ed25519.NewKeyFromSeed(seed), message)
		outputs = append(outputs, signaturePath(output))
	} else if _, err := os.Lstat(signaturePath(output)); !errors.Is(err, os.ErrNotExist) {
		return errors.New("unsigned generation must not leave an existing signature")
	}
	var changes []byte
	if opts.Old != "" {
		inputs = append(inputs, opts.Old)
		old, err := readLimited(opts.Old, maxBundle)
		if err != nil {
			return err
		}
		changes, err = rootDiff(old, canonical)
		if err != nil {
			return err
		}
	}
	if err := guardOutputs(inputs, outputs); err != nil {
		return err
	}
	for i, data := range [][]byte{canonical, encoded, signature} {
		if i >= len(outputs) {
			break
		}
		if err := os.WriteFile(outputs[i], data, 0644); err != nil {
			return err
		}
	}
	if len(changes) != 0 {
		_, err = report.Write(changes)
	}
	return err
}

func rootIdentities(data []byte) (map[string]string, error) {
	canonical, _, err := normalize(data, maxRoots)
	if err != nil {
		return nil, err
	}
	roots := map[string]string{}
	for len(canonical) > 0 {
		block, rest := pem.Decode(canonical)
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		roots[hash(block.Bytes)] = cert.Subject.String()
		canonical = rest
	}
	return roots, nil
}

func rootDiff(old, new []byte) ([]byte, error) {
	before, err := rootIdentities(old)
	if err != nil {
		return nil, err
	}
	after, err := rootIdentities(new)
	if err != nil {
		return nil, err
	}
	var lines []string
	for fingerprint, subject := range before {
		if _, ok := after[fingerprint]; !ok {
			kind := "removed"
			for candidate, other := range after {
				if other == subject {
					if _, existed := before[candidate]; !existed {
						kind = "replaced-old"
						break
					}
				}
			}
			lines = append(lines, fmt.Sprintf("%s %s %q\n", kind, fingerprint, subject))
		}
	}
	for fingerprint, subject := range after {
		if _, ok := before[fingerprint]; !ok {
			kind := "added"
			for candidate, other := range before {
				if other == subject {
					if _, remains := after[candidate]; !remains {
						kind = "replaced-new"
						break
					}
				}
			}
			lines = append(lines, fmt.Sprintf("%s %s %q\n", kind, fingerprint, subject))
		}
	}
	sort.Strings(lines)
	return []byte(strings.Join(lines, "")), nil
}

func within(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func keygen(directory string) error {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return err
	}
	real, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	cwd, err = filepath.EvalSymlinks(cwd)
	if err != nil {
		return err
	}
	if within(real, cwd) {
		return errors.New("key directory must be outside the checkout")
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("key directory must be a real empty directory")
	}
	for parent := real; ; parent = filepath.Dir(parent) {
		if _, err := os.Lstat(filepath.Join(parent, ".git")); err == nil {
			return errors.New("key directory must not be in a repository")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if filepath.Dir(parent) == parent {
			break
		}
	}
	entries, err := os.ReadDir(real)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("key directory is not empty")
	}
	if err := os.Chmod(real, 0700); err != nil {
		return err
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	for _, file := range []struct {
		name string
		data []byte
		mode os.FileMode
	}{
		{"private.seed", private.Seed(), 0600}, {"public.key", public, 0644},
	} {
		out, err := os.OpenFile(filepath.Join(real, file.name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, file.mode)
		if err != nil {
			return err
		}
		_, writeErr := out.Write(file.data)
		closeErr := out.Close()
		if err := errors.Join(writeErr, closeErr); err != nil {
			return err
		}
	}
	return nil
}
