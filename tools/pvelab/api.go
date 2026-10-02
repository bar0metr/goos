package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const apiRequestTimeout = 15 * time.Second

// API is the small subset of the Proxmox VE REST API pvelab uses. The
// Authorization header is built once and never logged; every error that
// leaves this file passes through redact.
type API struct {
	base    string
	auth    string
	secrets []string
	node    string
	client  *http.Client
	logf    func(format string, args ...any)
}

// APIError is a non-2xx answer. Proxmox puts its message in the status line
// and per-parameter problems in the errors object.
type APIError struct {
	Method string
	Path   string
	Status int
	Reason string
	Fields map[string]string
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("%s %s: %d %s", e.Method, e.Path, e.Status, e.Reason)
	if len(e.Fields) > 0 {
		keys := make([]string, 0, len(e.Fields))
		for k := range e.Fields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			msg += fmt.Sprintf("; %s: %s", k, strings.TrimSpace(e.Fields[k]))
		}
	}
	return msg
}

// isMissing reports an answer that means the object does not exist.
func isMissing(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.Status == http.StatusNotFound {
		return true
	}
	reason := strings.ToLower(apiErr.Reason)
	return strings.Contains(reason, "does not exist") || strings.Contains(reason, "no such")
}

func NewAPI(cfg *Config, logf func(string, ...any)) (*API, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("PVE_CA_FILE: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("PVE_CA_FILE: no PEM certificates in %s", cfg.CAFile)
		}
		tlsConfig.RootCAs = pool
	}
	if cfg.TLSInsecure {
		tlsConfig.InsecureSkipVerify = true
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &API{
		base:    fmt.Sprintf("https://%s/api2/json", hostPort(cfg.Host, cfg.Port)),
		auth:    "PVEAPIToken=" + cfg.TokenID + "=" + string(cfg.TokenSecret),
		secrets: []string{string(cfg.TokenSecret)},
		node:    cfg.Node,
		client:  &http.Client{Transport: transport},
		logf:    logf,
	}, nil
}

func hostPort(host string, port int) string {
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return host + ":" + strconv.Itoa(port)
}

// redact removes every credential from text.
func redact(text string, secrets ...string) string {
	for _, s := range secrets {
		if s != "" {
			text = strings.ReplaceAll(text, s, redacted)
		}
	}
	return text
}

type redactedError struct{ msg string }

func (e redactedError) Error() string { return e.msg }

func (a *API) clean(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		apiErr.Reason = redact(apiErr.Reason, a.secrets...)
		for k, v := range apiErr.Fields {
			apiErr.Fields[k] = redact(v, a.secrets...)
		}
		return apiErr
	}
	return redactedError{redact(err.Error(), a.secrets...)}
}

// call performs one request. form is sent as the query for GET and DELETE
// and as an urlencoded body otherwise; out receives the "data" member.
func (a *API) call(ctx context.Context, method, path string, form url.Values, out any) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, apiRequestTimeout)
		defer cancel()
	}
	target := a.base + path
	var body io.Reader
	if len(form) > 0 {
		if method == http.MethodGet || method == http.MethodDelete {
			target += "?" + form.Encode()
		} else {
			body = strings.NewReader(form.Encode())
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return a.clean(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return a.send(req, method, path, out)
}

func (a *API) send(req *http.Request, method, path string, out any) error {
	req.Header.Set("Authorization", a.auth)
	a.logf("api %s %s", method, path)
	resp, err := a.client.Do(req)
	if err != nil {
		return a.clean(fmt.Errorf("%s %s: %w", method, path, err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return a.clean(fmt.Errorf("%s %s: reading answer: %w", method, path, err))
	}
	return a.clean(decodeEnvelope(method, path, resp.StatusCode, resp.Status, raw, out))
}

// decodeEnvelope turns a Proxmox answer into out or an *APIError.
func decodeEnvelope(method, path string, code int, status string, raw []byte, out any) error {
	var envelope struct {
		Data   json.RawMessage   `json:"data"`
		Errors map[string]string `json:"errors"`
	}
	jsonErr := json.Unmarshal(raw, &envelope)
	if code < 200 || code > 299 {
		reason := strings.TrimSpace(strings.TrimPrefix(status, strconv.Itoa(code)))
		return &APIError{Method: method, Path: path, Status: code, Reason: reason, Fields: envelope.Errors}
	}
	if jsonErr != nil {
		return fmt.Errorf("%s %s: malformed answer: %w", method, path, jsonErr)
	}
	if out == nil || len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return nil
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("%s %s: unexpected answer: %w", method, path, err)
	}
	return nil
}

func esc(s string) string { return url.PathEscape(s) }

func (a *API) nodePath(format string, args ...any) string {
	return "/nodes/" + esc(a.node) + fmt.Sprintf(format, args...)
}

type Version struct {
	Version string `json:"version"`
	Release string `json:"release"`
}

func (a *API) Version(ctx context.Context) (Version, error) {
	var v Version
	err := a.call(ctx, http.MethodGet, "/version", nil, &v)
	return v, err
}

type NodeEntry struct {
	Node   string `json:"node"`
	Status string `json:"status"`
}

func (a *API) Nodes(ctx context.Context) ([]NodeEntry, error) {
	var nodes []NodeEntry
	err := a.call(ctx, http.MethodGet, "/nodes", nil, &nodes)
	return nodes, err
}

type NodeStatus struct {
	PVEVersion string `json:"pveversion"`
	KVersion   string `json:"kversion"`
	CPUInfo    struct {
		Model   string `json:"model"`
		CPUs    int    `json:"cpus"`
		Sockets int    `json:"sockets"`
		Cores   int    `json:"cores"`
		Flags   string `json:"flags,omitempty"`
	} `json:"cpuinfo"`
}

func (a *API) NodeStatus(ctx context.Context) (NodeStatus, error) {
	var s NodeStatus
	err := a.call(ctx, http.MethodGet, a.nodePath("/status"), nil, &s)
	s.CPUInfo.Flags = "" // long and adds nothing the model does not say
	return s, err
}

type StorageStatus struct {
	Type    string `json:"type"`
	Content string `json:"content"`
	Active  int    `json:"active"`
	Enabled int    `json:"enabled"`
}

func (a *API) StorageStatus(ctx context.Context, storage string) (StorageStatus, error) {
	var s StorageStatus
	err := a.call(ctx, http.MethodGet, a.nodePath("/storage/%s/status", esc(storage)), nil, &s)
	return s, err
}

type Volume struct {
	Volid string  `json:"volid"`
	VMID  flexInt `json:"vmid"`
}

func (a *API) StorageContent(ctx context.Context, storage, content string) ([]Volume, error) {
	form := url.Values{}
	if content != "" {
		form.Set("content", content)
	}
	var vols []Volume
	err := a.call(ctx, http.MethodGet, a.nodePath("/storage/%s/content", esc(storage)), form, &vols)
	return vols, err
}

func (a *API) Pool(ctx context.Context, pool string) error {
	return a.call(ctx, http.MethodGet, "/pools/"+esc(pool), nil, nil)
}

// Permissions returns the privileges the token holds on path, inheritance
// included.
func (a *API) Permissions(ctx context.Context, path string) (map[string]bool, error) {
	var tree map[string]map[string]int
	if err := a.call(ctx, http.MethodGet, "/access/permissions", url.Values{"path": {path}}, &tree); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for priv, v := range tree[path] {
		out[priv] = v != 0
	}
	return out, nil
}

// flexInt accepts a JSON number or a numeric string; Proxmox uses both.
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("not an integer: %s", b)
	}
	*f = flexInt(n)
	return nil
}

func (a *API) NextID(ctx context.Context) (int, error) {
	var id flexInt
	err := a.call(ctx, http.MethodGet, "/cluster/nextid", nil, &id)
	return int(id), err
}

// ClusterVM is a VM as /cluster/resources lists it; it is the only place
// that names a VM's pool.
type ClusterVM struct {
	VMID   flexInt `json:"vmid"`
	Name   string  `json:"name"`
	Node   string  `json:"node"`
	Pool   string  `json:"pool"`
	Status string  `json:"status"`
}

func (a *API) ClusterVM(ctx context.Context, vmid int) (*ClusterVM, error) {
	var vms []ClusterVM
	if err := a.call(ctx, http.MethodGet, "/cluster/resources", url.Values{"type": {"vm"}}, &vms); err != nil {
		return nil, err
	}
	for i := range vms {
		if int(vms[i].VMID) == vmid {
			return &vms[i], nil
		}
	}
	return nil, nil
}

// upid returns the task ID an asynchronous call answered with, or "".
func (a *API) upidCall(ctx context.Context, method, path string, form url.Values) (string, error) {
	var upid *string
	if err := a.call(ctx, method, path, form, &upid); err != nil {
		return "", err
	}
	if upid == nil {
		return "", nil
	}
	return *upid, nil
}

func (a *API) CreateVM(ctx context.Context, form url.Values) (string, error) {
	return a.upidCall(ctx, http.MethodPost, a.nodePath("/qemu"), form)
}

func (a *API) VMConfig(ctx context.Context, vmid int) (map[string]string, error) {
	var raw map[string]any
	if err := a.call(ctx, http.MethodGet, a.nodePath("/qemu/%d/config", vmid), nil, &raw); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		switch v := v.(type) {
		case string:
			out[k] = v
		case float64:
			out[k] = strconv.FormatFloat(v, 'f', -1, 64)
		default:
			b, _ := json.Marshal(v)
			out[k] = string(b)
		}
	}
	return out, nil
}

func (a *API) SetVMConfig(ctx context.Context, vmid int, form url.Values) (string, error) {
	return a.upidCall(ctx, http.MethodPost, a.nodePath("/qemu/%d/config", vmid), form)
}

type VMStatus struct {
	Status    string `json:"status"`
	QMPStatus string `json:"qmpstatus"`
	Name      string `json:"name"`
}

func (a *API) VMStatus(ctx context.Context, vmid int) (VMStatus, error) {
	var s VMStatus
	err := a.call(ctx, http.MethodGet, a.nodePath("/qemu/%d/status/current", vmid), nil, &s)
	return s, err
}

func (a *API) VMAction(ctx context.Context, vmid int, action string) (string, error) {
	return a.upidCall(ctx, http.MethodPost, a.nodePath("/qemu/%d/status/%s", vmid, action), nil)
}

func (a *API) DestroyVM(ctx context.Context, vmid int) (string, error) {
	form := url.Values{"purge": {"1"}, "destroy-unreferenced-disks": {"1"}}
	return a.upidCall(ctx, http.MethodDelete, a.nodePath("/qemu/%d", vmid), form)
}

func (a *API) DeleteVolume(ctx context.Context, storage, volid string) (string, error) {
	return a.upidCall(ctx, http.MethodDelete, a.nodePath("/storage/%s/content/%s", esc(storage), esc(volid)), nil)
}

// UploadISO streams path into storage as filename. Proxmox refuses chunked
// uploads, so the multipart body is framed with an exact length. Proxmox
// checks the SHA-256 itself before it accepts the file.
func (a *API) UploadISO(ctx context.Context, storage, filename, path, sha256 string, progress func(sent int64)) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	// pveproxy takes the first part named "filename" as the file, so the
	// file part is the only one with that name; the checksum follows it.
	var head bytes.Buffer
	mw := multipart.NewWriter(&head)
	if err := mw.WriteField("content", "iso"); err != nil {
		return "", err
	}
	if _, err := mw.CreateFormFile("filename", filename); err != nil {
		return "", err
	}
	prefix := append([]byte(nil), head.Bytes()...)
	head.Reset()
	for _, field := range [][2]string{{"checksum-algorithm", "sha256"}, {"checksum", sha256}} {
		if err := mw.WriteField(field[0], field[1]); err != nil {
			return "", err
		}
	}
	if err := mw.Close(); err != nil {
		return "", err
	}
	suffix := head.Bytes()
	body := io.MultiReader(bytes.NewReader(prefix), &countingReader{r: io.LimitReader(file, info.Size()), report: progress}, bytes.NewReader(suffix))

	path = a.nodePath("/storage/%s/upload", esc(storage))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+path, body)
	if err != nil {
		return "", a.clean(err)
	}
	req.ContentLength = int64(len(prefix)) + info.Size() + int64(len(suffix))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	var upid *string
	if err := a.send(req, http.MethodPost, path, &upid); err != nil {
		return "", err
	}
	if upid == nil {
		return "", nil
	}
	return *upid, nil
}

type countingReader struct {
	r      io.Reader
	n      int64
	report func(int64)
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.report != nil && n > 0 {
		c.report(c.n)
	}
	return n, err
}

// jsonIndent is used for the run files.
func jsonIndent(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return buf.Bytes()
}
