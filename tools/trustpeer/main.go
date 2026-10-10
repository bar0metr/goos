// trustpeer is a host-only standard-library TLS server for signed trust tests.
package main

import (
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"goos/tools/tlsfixture"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "trustpeer:", err)
		os.Exit(1)
	}
}

func server(directory, mode string) (*http.Server, error) {
	var cert tls.Certificate
	var err error
	switch mode {
	case "bootstrap":
		cert, err = tlsfixture.Certificate("valid")
	case "newroot":
		cert, err = tls.LoadX509KeyPair(filepath.Join(directory, "newroot-cert.pem"), filepath.Join(directory, "newroot-key-testonly.pem"))
	default:
		return nil, fmt.Errorf("unknown mode %q", mode)
	}
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	if mode == "bootstrap" {
		for _, generation := range []string{"update", "rollback", "badsig"} {
			for _, extension := range []string{"pem", "json", "sig"} {
				name := generation + "/ca-certificates." + extension
				data, err := os.ReadFile(filepath.Join(directory, name))
				if err != nil {
					return nil, err
				}
				files["/"+name] = data
			}
		}
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "HEAD" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var data []byte
		if r.URL.Path == "/hello" {
			data = []byte("hello")
			if mode == "newroot" {
				data = []byte("new-root-ok")
			}
		} else {
			var ok bool
			data, ok = files[r.URL.Path]
			if !ok {
				http.NotFound(w, r)
				return
			}
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusOK)
		if r.Method != "HEAD" {
			_, _ = w.Write(data)
		}
	})
	return &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 10 * time.Second, TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, NextProtos: []string{"http/1.1"}}}, nil
}

func run(args []string) error {
	flags := flag.NewFlagSet("trustpeer", flag.ContinueOnError)
	listen := flags.String("listen", "127.0.0.1:0", "loopback TCP listener")
	fixtures := flags.String("fixtures", "", "host trustfixture output directory")
	mode := flags.String("mode", "bootstrap", "bootstrap or newroot")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *fixtures == "" {
		return errors.New("-fixtures DIR required; no positional arguments")
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return errors.New("listener must use a numeric loopback address")
	}
	srv, err := server(*fixtures, *mode)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	defer srv.Close()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(tls.NewListener(listener, srv.TLSConfig)) }()
	fmt.Printf("trustpeer: listening %s mode=%s\n", listener.Addr(), *mode)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)
	select {
	case <-signals:
		return srv.Close()
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
}
