package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"time"
)

// portQueue bounds the frames waiting for one slow guest; more are dropped.
const portQueue = 256

type port struct {
	conn net.Conn
	out  chan []byte
}

// server is the hub: it floods each guest's frames to the other guests and
// hands them to the host, whose replies go to every guest.
type server struct {
	host     *host
	listener net.Listener
	socket   string
	quit     chan struct{}
	quitOnce sync.Once

	mu    sync.Mutex
	ports map[*port]struct{}
}

func listen(socket string, h *host) (*server, error) {
	if info, err := os.Lstat(socket); err == nil && info.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(socket)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return nil, err
	}
	s := &server{host: h, listener: listener, socket: socket, quit: make(chan struct{}), ports: map[*port]struct{}{}}
	h.later = func(delay time.Duration, frames [][]byte) {
		time.AfterFunc(delay, func() {
			for _, frame := range frames {
				s.send(frame, nil)
			}
		})
	}
	return s, nil
}

func (s *server) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.guest(conn)
	}
}

func (s *server) guest(conn net.Conn) {
	p := &port{conn: conn, out: make(chan []byte, portQueue)}
	s.mu.Lock()
	s.ports[p] = struct{}{}
	s.mu.Unlock()
	go func() {
		for frame := range p.out {
			if writeFrame(conn, frame) != nil {
				conn.Close()
				for range p.out {
				}
				return
			}
		}
	}()
	for {
		frame, err := readFrame(conn)
		if err != nil {
			break
		}
		s.send(frame, p)
		for _, reply := range s.host.handle(frame) {
			s.send(reply, nil)
		}
	}
	s.mu.Lock()
	if _, ok := s.ports[p]; ok {
		delete(s.ports, p)
		close(p.out)
	}
	s.mu.Unlock()
	conn.Close()
}

// send queues frame for every guest except the one it came from.
func (s *server) send(frame []byte, except *port) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for p := range s.ports {
		if p == except {
			continue
		}
		select {
		case p.out <- frame:
		default:
		}
	}
}

func (s *server) guests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.ports)
}

// run executes one control line.
func (s *server) run(line string) {
	frames, quit, err := s.host.command(line)
	if err != nil {
		fmt.Fprintln(os.Stderr, "netpeer:", err)
		return
	}
	for _, frame := range frames {
		s.send(frame, nil)
	}
	if quit {
		s.quitOnce.Do(func() { close(s.quit) })
	}
}

// close stops accepting, drops every guest and removes the socket.
func (s *server) close() {
	s.listener.Close()
	s.mu.Lock()
	for p := range s.ports {
		delete(s.ports, p)
		close(p.out)
		p.conn.Close()
	}
	s.mu.Unlock()
	_ = os.Remove(s.socket)
}

// prepareControl makes path a FIFO when nothing is there yet.
func prepareControl(path string) error {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return syscall.Mkfifo(path, 0o600)
	} else if err != nil {
		return err
	}
	return nil
}

// control reads commands from path. A FIFO is reopened after each EOF, so
// every 'echo cmd > fifo' is read. A regular file is followed as it grows.
func (s *server) control(path string) {
	info, err := os.Stat(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "netpeer:", err)
		return
	}
	if info.Mode()&os.ModeNamedPipe != 0 {
		for {
			file, err := os.Open(path)
			if err != nil {
				fmt.Fprintln(os.Stderr, "netpeer:", err)
				return
			}
			s.readCommands(file)
			file.Close()
		}
	}
	file, err := os.Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "netpeer:", err)
		return
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	pending := ""
	for {
		text, err := reader.ReadString('\n')
		pending += text
		if err == nil {
			s.run(pending)
			pending = ""
			continue
		}
		if err != io.EOF {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (s *server) readCommands(r io.Reader) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		s.run(scanner.Text())
	}
}
