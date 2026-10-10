// tlscompat prepares scheduler-free native overlays for pinned Go TLS.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"goos/tools/internal/compatroot"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const goVersion = "go1.25.5"
const connPin = "f5178241bea60da9af09ef0dd317354cea88e6c37ada835d43eceae98b520e2a"
const cachePin = "553ad7a40a9b198bea838833e3e5ed5660ec9b031ad75f58a9ea119e44bd3bfb"
const messagesPin = "fc837bf2986c3b1464d8ded6291517d55064c65a8ec294b6be344da7ca6ac81d"
const verifyPin = "8eb4bf09211ed11f5a25469af03e8d46a8330c02730d78a865f85e0fd0e6d8f3"
const defaultsPin = "377a0d907dd852ba9385b993b2f4c524a9dc9953e90f56c6d4d638bf6728833a"
const randPin = "086a516abf3977c155ab6d25538975043119c99775590983b15b9b258068de71"
const uefiPin = "e17517d3eca2177d9d4094abcc35347c3f650d5e52d1df92e3dd45ebe8b0f393"
const baremetalPin = "111e71bb241f682738895ec039dec7115b0ee16183fb240d42017545027b2e1f"
const manifestName = "tlscompat.json"

const cancellationBlock = `	handshakeCtx, cancel := context.WithCancel(ctx)
	// Note: defer this before starting the "interrupter" goroutine
	// so that we can tell the difference between the input being canceled and
	// this cancellation. In the former case, we need to close the connection.
	defer cancel()

	if c.quic != nil {
		c.quic.cancelc = handshakeCtx.Done()
		c.quic.cancel = cancel
	} else if ctx.Done() != nil {
		// Start the "interrupter" goroutine, if this context might be canceled.
		// (The background context cannot).
		//
		// The interrupter goroutine waits for the input context to be done and
		// closes the connection if this happens before the function returns.
		done := make(chan struct{})
		interruptRes := make(chan error, 1)
		defer func() {
			close(done)
			if ctxErr := <-interruptRes; ctxErr != nil {
				// Return context error to user.
				ret = ctxErr
			}
		}()
		go func() {
			select {
			case <-handshakeCtx.Done():
				// Close the connection, discarding the error
				_ = c.conn.Close()
				interruptRes <- handshakeCtx.Err()
			case <-done:
				interruptRes <- nil
			}
		}()
	}

`
const cancellationReplacement = `	if c.quic != nil || ctx.Done() != nil {
		return errors.New("tls: cancelable contexts and QUIC require a scheduler")
	}
	handshakeCtx := ctx

`

type pins struct{ Conn, Cache, Rand, Messages, Verify, Defaults string }
type runtimeManifest struct {
	TinyGoVersion    string `json:"tinygo_version"`
	SourceUEFI       string `json:"source_runtime_uefi_sha256"`
	SourceBaremetal  string `json:"source_baremetal_sha256"`
	Backend          string `json:"backend_sha256"`
	PatchedBaremetal string `json:"patched_baremetal_sha256"`
}
type manifest struct {
	GoVersion       string            `json:"go_version"`
	GoVersionFile   string            `json:"go_version_file_sha256"`
	TinyGoVersion   string            `json:"tinygo_version"`
	RuntimeManifest string            `json:"cryptocompat_manifest_sha256"`
	Sources         map[string]string `json:"source_sha256"`
	Adapters        map[string]string `json:"adapter_sha256"`
	Outputs         map[string]string `json:"output_sha256"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "tlscompat:", err)
		os.Exit(1)
	}
}
func run(args []string) error {
	if len(args) != 5 {
		return errors.New("usage: tlscompat SOURCE_GOROOT PREPARED_TINYGOROOT EMPTY_OUTPUT_GOROOT SYSRAND_ADAPTER TLS_CACHE_ADAPTER")
	}
	for _, path := range []string{args[1], args[2]} {
		abs, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		real, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return err
		}
		info, err := os.Lstat(abs)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !compatroot.Allowed(real) {
			return errors.New("prepared and output roots must be real directories under /Volumes/Data/tmp or GoOS build output")
		}
	}
	return prepare(args[0], args[1], args[2], args[3], args[4], pins{Conn: connPin, Cache: cachePin, Rand: randPin, Messages: messagesPin, Verify: verifyPin, Defaults: defaultsPin})
}
func hash(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func empty(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("output must be a real empty directory")
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("output is not empty")
	}
	return nil
}
func patchConn(data []byte) ([]byte, error) {
	if bytes.Count(data, []byte(cancellationBlock)) != 1 || bytes.Count(data, []byte("handshakeCtx, cancel := context.WithCancel(ctx)")) != 1 {
		return nil, errors.New("TLS handshake cancellation block does not match exactly once")
	}
	return bytes.Replace(data, []byte(cancellationBlock), []byte(cancellationReplacement), 1), nil
}

const tls13CertificateAppend = "\t\tcertificate.Certificate = append(certificate.Certificate, cert)"
const tls12CertificateScan = "\t\tif uint32(len(d)) < 3+certLen {"
const buildChainsEntry = "func (c *Certificate) buildChains(currentChain []*Certificate, sigChecks *int, opts *VerifyOptions) (chains [][]*Certificate, err error) {\n"

func patchCertificates(data []byte) ([]byte, error) {
	if bytes.Count(data, []byte(tls13CertificateAppend)) != 1 || bytes.Count(data, []byte(tls12CertificateScan)) != 1 || bytes.Contains(data, []byte("len(certificate.Certificate) >= 8")) || bytes.Contains(data, []byte("numCerts >= 8")) {
		return nil, errors.New("TLS certificate allocation anchors must match exactly once")
	}
	guard13 := "\t\tif len(certificate.Certificate) >= 8 || len(cert) == 0 || len(cert) > 32768 {\n\t\t\treturn false\n\t\t}\n" + tls13CertificateAppend
	guard12 := "\t\tif numCerts >= 8 || certLen == 0 || certLen > 32768 {\n\t\t\treturn false\n\t\t}\n" + tls12CertificateScan
	data = bytes.Replace(data, []byte(tls13CertificateAppend), []byte(guard13), 1)
	return bytes.Replace(data, []byte(tls12CertificateScan), []byte(guard12), 1), nil
}

const noAESCipherSuites = `var defaultCipherSuitesTLS13NoAES = []uint16{
	TLS_CHACHA20_POLY1305_SHA256,
	TLS_AES_128_GCM_SHA256,
	TLS_AES_256_GCM_SHA384,
}`

const aesFirstCipherSuites = `var defaultCipherSuitesTLS13NoAES = []uint16{
	TLS_AES_128_GCM_SHA256,
	TLS_AES_256_GCM_SHA384,
	TLS_CHACHA20_POLY1305_SHA256,
}`

// patchCipherSuites changes only the no-AES CPU preference ordering. All
// upstream TLS 1.3 suites remain available; native software AES is preferred
// so the integration gate deterministically negotiates AES-128-GCM.
func patchCipherSuites(data []byte) ([]byte, error) {
	if bytes.Count(data, []byte(noAESCipherSuites)) != 1 || bytes.Contains(data, []byte(aesFirstCipherSuites)) {
		return nil, errors.New("TLS 1.3 no-AES cipher preference does not match exactly once")
	}
	return bytes.Replace(data, []byte(noAESCipherSuites), []byte(aesFirstCipherSuites), 1), nil
}

func patchVerify(data []byte) ([]byte, error) {
	if bytes.Count(data, []byte(buildChainsEntry)) != 1 || bytes.Contains(data, []byte("x509: candidate chain depth exceeds 8")) {
		return nil, errors.New("X.509 buildChains entry must match exactly once")
	}
	guarded := buildChainsEntry + "\tif len(currentChain) >= 8 {\n\t\treturn nil, errors.New(\"x509: candidate chain depth exceeds 8\")\n\t}\n"
	return bytes.Replace(data, []byte(buildChainsEntry), []byte(guarded), 1), nil
}

func adapter(data []byte, pkg string) error {
	f, err := parser.ParseFile(token.NewFileSet(), "adapter.go", data, 0)
	if err != nil {
		return err
	}
	if f.Name.Name != pkg {
		return fmt.Errorf("adapter package must be %s", pkg)
	}
	for _, im := range f.Imports {
		name, _ := strconv.Unquote(im.Path.Value)
		if name == "time" || name == "context" {
			return errors.New("adapter cannot import timers or cancellation")
		}
	}
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "init" {
			return errors.New("adapter cannot define init")
		}
		if gd, ok := decl.(*ast.GenDecl); ok && gd.Tok == token.VAR {
			for _, spec := range gd.Specs {
				if len(spec.(*ast.ValueSpec).Values) != 0 {
					return errors.New("adapter cannot initialize package variables")
				}
			}
		}
	}
	hasGo := false
	ast.Inspect(f, func(n ast.Node) bool {
		if _, ok := n.(*ast.GoStmt); ok {
			hasGo = true
		}
		return true
	})
	if hasGo {
		return errors.New("adapter cannot start goroutines")
	}
	return nil
}
func license(source []byte) []byte {
	if i := bytes.Index(source, []byte("\n\n")); i >= 0 {
		return source[:i+2]
	}
	return nil
}
func production(dir string) (map[string][]byte, error) {
	files := map[string][]byte{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		if bytes.Contains(data, []byte("//go:build ignore\n")) {
			continue
		}
		files[name] = data
	}
	if len(files) == 0 {
		return nil, errors.New("production source tree is empty")
	}
	return files, nil
}
func inspectTinyGo(root string) ([]byte, runtimeManifest, error) {
	var record runtimeManifest
	data, err := os.ReadFile(filepath.Join(root, "cryptocompat.json"))
	if err != nil {
		return nil, record, err
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, record, err
	}
	if record.TinyGoVersion != "0.42.0" || record.SourceUEFI != uefiPin || record.SourceBaremetal != baremetalPin {
		return nil, record, errors.New("prepared TinyGo metadata is not pinned 0.42.0")
	}
	for path, want := range map[string]string{"runtime_uefi.go": record.Backend, "baremetal.go": record.PatchedBaremetal} {
		got, err := os.ReadFile(filepath.Join(root, "src", "runtime", path))
		if err != nil {
			return nil, record, err
		}
		if want == "" || hash(got) != want {
			return nil, record, errors.New("prepared runtime hash mismatch")
		}
	}
	for _, path := range []string{"src", "src/crypto", "src/crypto/tls"} {
		info, err := os.Lstat(filepath.Join(root, path))
		if err != nil {
			return nil, record, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, record, errors.New("prepared TLS ancestors must be real directories")
		}
	}
	bin, err := os.Readlink(filepath.Join(root, "bin"))
	if err != nil {
		return nil, record, err
	}
	if !filepath.IsAbs(bin) || filepath.Base(bin) != "bin" {
		return nil, record, errors.New("prepared root bin is not the source symlink")
	}
	sourceTLS := filepath.Join(filepath.Dir(bin), "src", "crypto", "tls")
	tlsDir := filepath.Join(root, "src", "crypto", "tls")
	entries, err := os.ReadDir(tlsDir)
	if err != nil {
		return nil, record, err
	}
	allowed := map[string]bool{"cipher_suites.go": true, "common.go": true, "ticket.go": true, "tls.go": true}
	if len(entries) != len(allowed) {
		return nil, record, errors.New("prepared TLS directory is not the original generated shim mirror")
	}
	for _, entry := range entries {
		if !allowed[entry.Name()] {
			return nil, record, errors.New("unrecognized file in prepared TLS directory")
		}
		target, err := os.Readlink(filepath.Join(tlsDir, entry.Name()))
		if err != nil || target != filepath.Join(sourceTLS, entry.Name()) {
			return nil, record, errors.New("prepared TLS entry is not its original source symlink")
		}
	}
	return data, record, nil
}
func prepare(source, tinygo, output, randPath, cachePath string, expected pins) (retErr error) {
	var err error
	source, err = filepath.Abs(source)
	if err != nil {
		return err
	}
	tinygo, err = filepath.Abs(tinygo)
	if err != nil {
		return err
	}
	output, err = filepath.Abs(output)
	if err != nil {
		return err
	}
	if err := empty(output); err != nil {
		return err
	}
	version, err := os.ReadFile(filepath.Join(source, "VERSION"))
	if err != nil {
		return err
	}
	if strings.SplitN(string(version), "\n", 2)[0] != goVersion {
		return errors.New("source GOROOT must be Go 1.25.5")
	}
	recordBytes, rt, err := inspectTinyGo(tinygo)
	if err != nil {
		return err
	}
	tlsFiles, err := production(filepath.Join(source, "src", "crypto", "tls"))
	if err != nil {
		return err
	}
	helperFiles, err := production(filepath.Join(source, "src", "crypto", "tls", "internal", "fips140tls"))
	if err != nil {
		return err
	}
	sysrand, err := os.ReadFile(filepath.Join(source, "src", "crypto", "internal", "sysrand", "rand.go"))
	if err != nil {
		return err
	}
	verify, err := os.ReadFile(filepath.Join(source, "src", "crypto", "x509", "verify.go"))
	if err != nil {
		return err
	}
	if hash(tlsFiles["conn.go"]) != expected.Conn || hash(tlsFiles["cache.go"]) != expected.Cache || hash(sysrand) != expected.Rand || hash(tlsFiles["handshake_messages.go"]) != expected.Messages || hash(verify) != expected.Verify || hash(tlsFiles["defaults.go"]) != expected.Defaults {
		return errors.New("Go source SHA256 does not match pinned inputs")
	}
	randomAdapter, err := os.ReadFile(randPath)
	if err != nil {
		return err
	}
	if err := adapter(randomAdapter, "sysrand"); err != nil {
		return err
	}
	cacheAdapter, err := os.ReadFile(cachePath)
	if err != nil {
		return err
	}
	if err := adapter(cacheAdapter, "tls"); err != nil {
		return err
	}
	patched, err := patchConn(tlsFiles["conn.go"])
	if err != nil {
		return err
	}
	patchedMessages, err := patchCertificates(tlsFiles["handshake_messages.go"])
	if err != nil {
		return err
	}
	patchedVerify, err := patchVerify(verify)
	if err != nil {
		return err
	}
	patchedDefaults, err := patchCipherSuites(tlsFiles["defaults.go"])
	if err != nil {
		return err
	}
	m := manifest{GoVersion: goVersion, GoVersionFile: hash(version), TinyGoVersion: rt.TinyGoVersion, RuntimeManifest: hash(recordBytes), Sources: map[string]string{}, Adapters: map[string]string{"sysrand": hash(randomAdapter), "tls_cache": hash(cacheAdapter)}, Outputs: map[string]string{}}
	for name, data := range tlsFiles {
		m.Sources["crypto/tls/"+name] = hash(data)
	}
	for name, data := range helperFiles {
		m.Sources["crypto/tls/internal/fips140tls/"+name] = hash(data)
	}
	m.Sources["crypto/internal/sysrand/rand.go"] = hash(sysrand)
	m.Sources["crypto/x509/verify.go"] = hash(verify)
	m.Outputs["crypto/x509/verify.go"] = hash(patchedVerify)
	tlsFiles["handshake_messages.go"] = patchedMessages
	tlsFiles["conn.go"] = patched
	tlsFiles["defaults.go"] = patchedDefaults
	tlsFiles["cache.go"] = append(append([]byte(nil), license(tlsFiles["cache.go"])...), cacheAdapter...)
	randomOutput := append(append([]byte(nil), license(sysrand)...), randomAdapter...)
	m.Outputs["crypto/internal/sysrand/rand.go"] = hash(randomOutput)
	if _, err := os.Lstat(filepath.Join(source, manifestName)); !errors.Is(err, os.ErrNotExist) {
		return errors.New("source GOROOT contains reserved manifest filename")
	}
	goStage, err := os.MkdirTemp(filepath.Dir(output), ".tlscompat-go-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(goStage)
	if err := mirror(source, goStage, false, "src"); err != nil {
		return err
	}
	if err := os.Mkdir(filepath.Join(goStage, "src"), 0755); err != nil {
		return err
	}
	if err := mirror(filepath.Join(source, "src"), filepath.Join(goStage, "src"), true); err != nil {
		return err
	}
	randDest := filepath.Join(goStage, "src", "crypto", "internal", "sysrand", "rand.go")
	if err := os.Remove(randDest); err != nil {
		return err
	}
	if err := os.WriteFile(randDest, randomOutput, 0644); err != nil {
		return err
	}
	verifyDest := filepath.Join(goStage, "src", "crypto", "x509", "verify.go")
	if err := os.Remove(verifyDest); err != nil {
		return err
	}
	if err := os.WriteFile(verifyDest, patchedVerify, 0644); err != nil {
		return err
	}
	tlsParent := filepath.Join(tinygo, "src", "crypto")
	tlsStage, err := os.MkdirTemp(tlsParent, ".tlscompat-stage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tlsStage)
	for name, data := range tlsFiles {
		if err := os.WriteFile(filepath.Join(tlsStage, name), data, 0644); err != nil {
			return err
		}
		m.Outputs["crypto/tls/"+name] = hash(data)
	}
	helperDest := filepath.Join(tlsStage, "internal", "fips140tls")
	if err := os.MkdirAll(helperDest, 0755); err != nil {
		return err
	}
	for name, data := range helperFiles {
		if err := os.WriteFile(filepath.Join(helperDest, name), data, 0644); err != nil {
			return err
		}
		m.Outputs["crypto/tls/internal/fips140tls/"+name] = hash(data)
	}
	licenseData, err := os.ReadFile(filepath.Join(source, "LICENSE"))
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tlsStage, "LICENSE"), licenseData, 0644); err != nil {
		return err
	}
	m.Sources["LICENSE"] = hash(licenseData)
	m.Outputs["crypto/tls/LICENSE"] = hash(licenseData)
	encoded, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(filepath.Join(goStage, manifestName), encoded, 0644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tlsStage, manifestName), encoded, 0644); err != nil {
		return err
	}
	if _, _, err := inspectTinyGo(tinygo); err != nil {
		return err
	}
	if err := empty(output); err != nil {
		return err
	}
	backup, err := os.MkdirTemp(tlsParent, ".tlscompat-original-")
	if err != nil {
		return err
	}
	if err := os.Remove(backup); err != nil {
		return err
	}
	tlsDest := filepath.Join(tlsParent, "tls")
	if err := os.Rename(tlsDest, backup); err != nil {
		return err
	}
	committed := false
	defer func() {
		if committed {
			os.RemoveAll(backup)
		} else {
			removeErr := os.RemoveAll(tlsDest)
			restoreErr := os.Rename(backup, tlsDest)
			retErr = errors.Join(retErr, removeErr, restoreErr)
		}
	}()
	if err := os.Rename(tlsStage, tlsDest); err != nil {
		return err
	}
	// macOS needs an absent destination; there is a brief non-atomic rename gap.
	if err := os.Remove(output); err != nil {
		return err
	}
	if err := os.Rename(goStage, output); err != nil {
		return errors.Join(err, os.Mkdir(output, 0755))
	}
	committed = true
	return nil
}
func mirror(source, dest string, recursive bool, excluded ...string) error {
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		skip := false
		for _, name := range excluded {
			skip = skip || entry.Name() == name
		}
		if skip {
			continue
		}
		from, to := filepath.Join(source, entry.Name()), filepath.Join(dest, entry.Name())
		if recursive {
			info, err := os.Stat(from)
			if err != nil {
				return err
			}
			if info.IsDir() {
				if err := os.Mkdir(to, 0755); err != nil {
					return err
				}
				if err := mirror(from, to, true); err != nil {
					return err
				}
				continue
			}
		}
		if err := os.Symlink(from, to); err != nil {
			return err
		}
	}
	return nil
}
