// Package trustupdate downloads bounded trust package files through fetch.
package trustupdate

import (
	"errors"
	"goos/userspace/fetch"
	"goos/userspace/network"
	"goos/userspace/truststore"
	"strconv"
	"strings"
)

func ValidateURL(base string) error {
	var url fetch.URL
	if len(strings.TrimRight(base, "/"))+len("/ca-certificates.json") > fetch.MaxURL {
		return errors.New("trust update URL leaves no room for package filenames")
	}
	if err := fetch.ParseURL([]byte(base), &url); err != nil || !url.Secure || strings.ContainsAny(base, "?#\r\n") {
		return errors.New("trust update URL must be an HTTPS base URL")
	}
	return nil
}

type boundedOutput struct {
	data  []byte
	limit int
}

func (o *boundedOutput) Write(data []byte) error {
	if len(data) > o.limit-len(o.data) {
		return errors.New("trust download exceeds size limit")
	}
	o.data = append(o.data, data...)
	return nil
}
func (*boundedOutput) Close() error { return nil }

type downloadEnv struct {
	fetch.Environment
	output *boundedOutput
}

func (e *downloadEnv) Stdout() fetch.Output { return e.output }
func (*downloadEnv) Open([]byte) (fetch.Output, error) {
	return nil, errors.New("trust download cannot open output files")
}
func (*downloadEnv) CheckResponse(status int) error {
	if status != 200 {
		return errors.New("trust download HTTP status " + strconv.Itoa(status))
	}
	return nil
}
func (e *downloadEnv) ConnectTLS(address network.IPv4, port uint16, host []byte) (fetch.Connection, error) {
	secure, ok := e.Environment.(fetch.SecureNetwork)
	if !ok {
		return nil, fetch.ErrTLSUnavailable
	}
	return secure.ConnectTLS(address, port, host)
}

// Download keeps the caller's current TLS roots for every component. The caller
// must authenticate the complete package with its immutable signing key next.
func Download(env fetch.Environment, base string) (bundle, meta, sig []byte, err error) {
	if err = ValidateURL(base); err != nil {
		return
	}
	base = strings.TrimRight(base, "/")
	read := func(name string, limit int) ([]byte, error) {
		output := &boundedOutput{limit: limit}
		options := fetch.Options{URL: []byte(base + "/" + name)}
		if err := fetch.Run(&options, &downloadEnv{Environment: env, output: output}); err != nil {
			return nil, err
		}
		return output.data, nil
	}
	meta, err = read("ca-certificates.json", truststore.MaxMetadata)
	if err != nil {
		return
	}
	if _, err = truststore.ParseMetadata(meta); err != nil {
		return
	}
	sig, err = read("ca-certificates.sig", 64)
	if err != nil {
		return
	}
	bundle, err = read("ca-certificates.pem", truststore.MaxBundle)
	return
}
