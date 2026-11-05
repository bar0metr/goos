#!/usr/bin/env python3
"""Host side of the host-guest network benchmark (pvelab linux-network-bench).

pvelab copies this file to the Proxmox node's staging directory of a run and
removes it with the directory. It needs only the Python 3 standard library.

  benchpeer.py serve PORT PIDFILE LIFETIME
      Serve TCP and UDP on PORT until SIGTERM or LIFETIME seconds pass. Each
      TCP connection starts with a nine-byte command: an opcode and a
      little-endian 64-bit count. 'S' reads count bytes and answers one byte,
      'G' sends count bytes, 'E' echoes count single bytes. The UDP socket
      echoes each datagram to its sender.

  benchpeer.py client ADDRESS PORT COUNT
      Make COUNT connections to netprobe remote-serve in the guest: connect,
      send one byte, read it back, close. Prints one bench line.
"""

import os
import signal
import socket
import struct
import sys
import threading
import time

CHUNK = 65536


def receive(conn, size):
    data = bytearray()
    while len(data) < size:
        part = conn.recv(size - len(data))
        if not part:
            raise EOFError("peer closed after %d of %d bytes" % (len(data), size))
        data += part
    return bytes(data)


def handle(conn):
    with conn:
        conn.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
        opcode, count = struct.unpack("<cQ", receive(conn, 9))
        if opcode == b"S":
            left = count
            while left > 0:
                part = conn.recv(min(CHUNK, left))
                if not part:
                    return
                left -= len(part)
            conn.sendall(b"k")
        elif opcode == b"G":
            block = bytes(CHUNK)
            left = count
            while left > 0:
                n = min(CHUNK, left)
                conn.sendall(block[:n])
                left -= n
            conn.shutdown(socket.SHUT_WR)
            while conn.recv(CHUNK):
                pass
            report_tcp_info(conn, "G")
        elif opcode == b"E":
            for _ in range(count):
                byte = conn.recv(1)
                if not byte:
                    return
                conn.sendall(byte)


def report_tcp_info(conn, what):
    """Log the sender's view of a finished stream: Linux struct tcp_info
    fields, among them the microseconds the guest's window limited it."""
    try:
        raw = conn.getsockopt(socket.IPPROTO_TCP, socket.TCP_INFO, 192)
    except OSError:
        return
    if len(raw) < 192:
        return
    u32 = struct.unpack_from("<24I", raw, 8)
    rto, snd_mss, unacked, lost, retrans = u32[0], u32[2], u32[4], u32[6], u32[7]
    rtt, snd_cwnd, total_retrans = u32[15], u32[18], u32[23]
    segs_out, segs_in = struct.unpack_from("<2I", raw, 136)
    busy, rwnd_limited, sndbuf_limited = struct.unpack_from("<3Q", raw, 168)
    sys.stderr.write(
        "tcpinfo %s rto=%d mss=%d unacked=%d lost=%d retrans=%d total-retrans=%d rtt=%d cwnd=%d "
        "segs-out=%d segs-in=%d busy-us=%d rwnd-limited-us=%d sndbuf-limited-us=%d\n"
        % (what, rto, snd_mss, unacked, lost, retrans, total_retrans, rtt, snd_cwnd,
           segs_out, segs_in, busy, rwnd_limited, sndbuf_limited))
    sys.stderr.flush()


def tcp_loop(listener):
    while True:
        conn, _ = listener.accept()
        threading.Thread(target=handle_quietly, args=(conn,), daemon=True).start()


def handle_quietly(conn):
    try:
        handle(conn)
    except (OSError, EOFError) as err:
        print("connection: %s" % err, file=sys.stderr, flush=True)


def udp_loop(sock):
    while True:
        data, peer = sock.recvfrom(65536)
        sock.sendto(data, peer)


def serve(port, pidfile, lifetime):
    listener = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    listener.bind(("0.0.0.0", port))
    listener.listen(16)
    udp = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    udp.bind(("0.0.0.0", port))
    with open(pidfile, "w") as f:
        f.write("%d\n" % os.getpid())
    signal.signal(signal.SIGTERM, lambda *_: os._exit(0))
    threading.Thread(target=tcp_loop, args=(listener,), daemon=True).start()
    threading.Thread(target=udp_loop, args=(udp,), daemon=True).start()
    print("benchpeer listening %d" % port, flush=True)
    time.sleep(lifetime)


def exchange(address, port):
    with socket.create_connection((address, port), timeout=5) as conn:
        conn.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
        conn.sendall(b"x")
        if conn.recv(1) != b"x":
            raise EOFError("no reply")


def client(address, port, count):
    # The guest server may still be starting. A refused first connection
    # retries for ten seconds and stays out of the timing.
    deadline = time.monotonic() + 10
    while True:
        try:
            exchange(address, port)
            break
        except ConnectionRefusedError:
            if time.monotonic() > deadline:
                raise
            time.sleep(0.1)
    total = 0.0
    for _ in range(count - 1):
        start = time.perf_counter()
        exchange(address, port)
        total += time.perf_counter() - start
    count -= 1
    print("bench remote-tcp-accept-close count=%d us-per-connection=%d" % (count, total * 1e6 / count), flush=True)


def main(argv):
    if len(argv) == 5 and argv[1] == "serve":
        serve(int(argv[2]), argv[3], float(argv[4]))
    elif len(argv) == 5 and argv[1] == "client":
        client(argv[2], int(argv[3]), int(argv[4]))
    else:
        print(__doc__, file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
