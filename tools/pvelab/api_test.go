package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testSecret = "11111111-2222-3333-4444-555555555555"

func testAPI(t *testing.T, handler http.HandlerFunc) *API {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	return &API{
		base:    server.URL + "/api2/json",
		auth:    "PVEAPIToken=u@pve!t=" + testSecret,
		secrets: []string{testSecret},
		node:    "pve01",
		client:  server.Client(),
		logf:    func(string, ...any) {},
	}
}

func TestDecodeEnvelope(t *testing.T) {
	var v Version
	if err := decodeEnvelope("GET", "/version", 200, "200 OK", []byte(`{"data":{"version":"7.4-16","release":"7.4"}}`), &v); err != nil {
		t.Fatal(err)
	}
	if v.Version != "7.4-16" {
		t.Errorf("version %q", v.Version)
	}
	err := decodeEnvelope("POST", "/nodes/n/qemu", 400, "400 Parameter verification failed.", []byte(`{"data":null,"errors":{"memory":"value must be at least 16"}}`), nil)
	apiErr, ok := err.(*APIError)
	if !ok || apiErr.Status != 400 || apiErr.Fields["memory"] == "" {
		t.Fatalf("got %#v", err)
	}
	if !strings.Contains(err.Error(), "memory: value must be at least 16") {
		t.Errorf("error %q lacks the field message", err)
	}
	if err := decodeEnvelope("GET", "/x", 200, "200 OK", []byte(`not json`), &v); err == nil {
		t.Error("malformed answer accepted")
	}
}

func TestFlexInt(t *testing.T) {
	var a, b flexInt
	if err := a.UnmarshalJSON([]byte(`"154"`)); err != nil || a != 154 {
		t.Errorf("string: %v %v", a, err)
	}
	if err := b.UnmarshalJSON([]byte(`154`)); err != nil || b != 154 {
		t.Errorf("number: %v %v", b, err)
	}
	if err := b.UnmarshalJSON([]byte(`"x"`)); err == nil {
		t.Error("accepted x")
	}
}

func TestIsMissing(t *testing.T) {
	if !isMissing(&APIError{Status: 500, Reason: "Configuration file 'nodes/n/qemu-server/9.conf' does not exist"}) {
		t.Error("missing config not recognised")
	}
	if !isMissing(&APIError{Status: 404}) {
		t.Error("404 not recognised")
	}
	if isMissing(&APIError{Status: 403, Reason: "Permission check failed"}) {
		t.Error("403 taken for missing")
	}
}

func TestAPIErrorsAreRedacted(t *testing.T) {
	api := testAPI(t, func(w http.ResponseWriter, r *http.Request) {
		// A server that echoes the credential back must not get it printed.
		w.WriteHeader(401)
		w.Write([]byte(`{"data":null,"errors":{"auth":"bad token ` + r.Header.Get("Authorization") + `"}}`))
	})
	_, err := api.Version(context.Background())
	if err == nil {
		t.Fatal("no error")
	}
	if strings.Contains(err.Error(), testSecret) {
		t.Errorf("secret in error: %v", err)
	}
}

func TestAPISendsTokenHeader(t *testing.T) {
	var got string
	api := testAPI(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.Write([]byte(`{"data":"155"}`))
	})
	id, err := api.NextID(context.Background())
	if err != nil || id != 155 {
		t.Fatalf("NextID = %d, %v", id, err)
	}
	if got != "PVEAPIToken=u@pve!t="+testSecret {
		t.Errorf("Authorization %q", got)
	}
}

func TestTaskOutcome(t *testing.T) {
	cases := []struct {
		s        TaskStatus
		done, ok bool
	}{
		{TaskStatus{Status: "running"}, false, true},
		{TaskStatus{Status: "stopped", ExitStatus: "OK"}, true, true},
		{TaskStatus{Status: "stopped", ExitStatus: "WARNINGS: 2"}, true, true},
		{TaskStatus{Status: "stopped", ExitStatus: "unable to create VM 9 - disk full"}, true, false},
		{TaskStatus{Status: "stopped"}, true, false},
	}
	for _, c := range cases {
		done, err := taskOutcome(c.s)
		if done != c.done || (err == nil) != c.ok {
			t.Errorf("%+v: done=%v err=%v", c.s, done, err)
		}
	}
}

func TestUPIDNode(t *testing.T) {
	node, err := upidNode("UPID:pve01:0000A1B2:01234567:6512ABCD:qmcreate:154:u@pve!t:")
	if err != nil || node != "pve01" {
		t.Errorf("node %q, %v", node, err)
	}
	if _, err := upidNode("garbage"); err == nil {
		t.Error("garbage accepted")
	}
}

func TestWaitTaskFailureCarriesLogTail(t *testing.T) {
	var polls atomic.Int32
	api := testAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/status"):
			if polls.Add(1) < 2 {
				w.Write([]byte(`{"data":{"status":"running"}}`))
				return
			}
			w.Write([]byte(`{"data":{"status":"stopped","exitstatus":"import failed"}}`))
		case strings.HasSuffix(r.URL.Path, "/log"):
			w.Write([]byte(`{"data":[{"n":1,"t":"starting"},{"n":2,"t":"error: no space left"}]}`))
		}
	})
	err := api.WaitTask(context.Background(), "UPID:pve01:1:2:3:qmimport:154:u@pve!t:")
	if err == nil || !strings.Contains(err.Error(), "import failed") || !strings.Contains(err.Error(), "no space left") {
		t.Fatalf("err = %v", err)
	}
}

func TestWaitTaskHonoursContext(t *testing.T) {
	api := testAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"status":"running"}}`))
	})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := api.WaitTask(ctx, "UPID:pve01:1:2:3:qmstart:154:u@pve!t:")
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("err = %v after %s", err, time.Since(start))
	}
}

func TestWaitTaskEmptyIsDone(t *testing.T) {
	api := testAPI(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected request") })
	if err := api.WaitTask(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
}

func TestUploadISOFraming(t *testing.T) {
	dir := t.TempDir()
	iso := dir + "/goos.iso"
	content := strings.Repeat("ISO9660", 1000)
	if err := os.WriteFile(iso, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	api := testAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if len(r.TransferEncoding) != 0 || r.ContentLength <= int64(len(content)) {
			t.Errorf("chunked or unsized upload: %v %d", r.TransferEncoding, r.ContentLength)
		}
		mr, err := r.MultipartReader()
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for {
			part, err := mr.NextPart()
			if err != nil {
				break
			}
			data, _ := io.ReadAll(part)
			names = append(names, part.FormName())
			if part.FormName() == "filename" && (part.FileName() != "x.iso" || string(data) != content) {
				t.Errorf("file part %q, %d bytes", part.FileName(), len(data))
			}
		}
		if strings.Join(names, ",") != "content,filename,checksum-algorithm,checksum" {
			t.Errorf("parts %v", names)
		}
		w.Write([]byte(`{"data":"UPID:pve01:1:2:3:imgcopy::u@pve!t:"}`))
	})
	var sent int64
	upid, err := api.UploadISO(context.Background(), "local", "x.iso", iso, "abc", func(n int64) { sent = n })
	if err != nil || !strings.HasPrefix(upid, "UPID:") || sent != int64(len(content)) {
		t.Fatalf("upid %q err %v sent %d", upid, err, sent)
	}
}
