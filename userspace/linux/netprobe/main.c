/*
 * netprobe checks the Linux IPv4 socket ABI one group at a time and prints
 * a line per group. The same binary runs on a Linux kernel, where every
 * line must read "<group> ok": that run is the reference GoOS must match.
 * The probe checks only results both systems must give. Where GoOS documents a
 * different bound (buffer sizes, the largest datagram, the backlog), the
 * probe checks the shape a Linux program may rely on, not the number.
 *
 * With no argument, or "all", it runs every loopback group over 127.0.0.1
 * and ends with "netprobe ok". A group name runs that group alone:
 *
 *   create    socket(): families, types, protocols, SOCK_CLOEXEC, SOCK_NONBLOCK
 *   sockaddr  short, exact, and oversized lengths; truncated name results
 *   bind      port 0, EADDRINUSE, EADDRNOTAVAIL, SO_REUSEADDR over TIME_WAIT
 *   listen    SO_ACCEPTCONN, accept and accept4 flags, an early disconnect
 *   connect   blocking and nonblocking connect, SO_ERROR, refusal
 *   stream    MSG_PEEK, MSG_DONTWAIT, MSG_WAITALL, FIONREAD, a large
 *             transfer, nonblocking writes until EAGAIN, EOF after data
 *   shutdown  SHUT_RD, SHUT_WR, SHUT_RDWR, repeated and invalid calls
 *   sigpipe   EPIPE with SIGPIPE handled, blocked, ignored, default, and
 *             MSG_NOSIGNAL
 *   poll      POLLIN, POLLOUT, POLLHUP, POLLRDHUP, POLLNVAL on sockets
 *   select    select and pselect6 ready sets, timeouts, a pselect6 mask
 *   udp       datagrams: sendto, recvfrom, sendmsg, recvmsg, connected
 *             filtering, truncation, MSG_TRUNC, MSG_PEEK, FIONREAD, EMSGSIZE
 *   dup       dup, F_DUPFD_CLOEXEC, shared status flags, fork, last close
 *   exec      a socket kept across execve and a SOCK_CLOEXEC one closed
 *   eintr     accept and read interrupted, restarted under SA_RESTART
 *   options   SOL_SOCKET, IPPROTO_TCP and IPPROTO_IP options
 *   file      fstat, lseek, fsync, and terminal ioctls on a socket
 *
 * Client and server modes serve the network gate, against a peer outside:
 *
 *   resolve NAME              print the IPv4 address of NAME
 *   fetch [-n] HOST PORT PATH HTTP/1.0 GET; print the status, body size,
 *                             and body SHA-256 (-n: nonblocking connect)
 *   connect HOST PORT MS      nonblocking connect, poll for MS, SO_ERROR
 *   reset HOST PORT           send "reset" to an echo peer that answers
 *                             with a TCP reset
 *   silent HOST PORT MS       SO_RCVTIMEO read from a peer that says nothing
 *   udp-echo HOST PORT        datagrams against a UDP echo service
 *   inherit HOST PORT         a TCP socket shared with a child and an
 *                             execve'd image, against a TCP echo service
 *   serve PORT FILE COUNT     answer COUNT HTTP requests with FILE, one
 *                             forked child per connection
 *   hold HOST PORT fail|survive SECONDS
 *                             hold sockets while a service is killed
 *
 * Benchmarks print "bench NAME key=value ..." lines and end with "bench ok":
 *
 *   bench [MiB]               loopback stream (32 MiB by default), request
 *                             and reply, connect and close, UDP, poll wake
 *   bench-stream MiB          the loopback stream alone
 *   bench-rr ROUNDS           the loopback request and reply alone
 *   stress-stream MiB COUNT   the loopback stream COUNT times, one
 *                             "stress N ok" line each, for a watchdog
 *   long-stream MiB           one connection carrying MiB, every byte
 *                             checked, a "long MiB=" line per 256 MiB
 *   remote ADDRESS PORT [MiB] streams both ways, request and reply, and UDP
 *                             against tools/pvelab/benchpeer.py
 *   remote-serve PORT COUNT   answer COUNT one-byte requests from a remote
 *                             client, one connection each
 *
 * The bundled musl has no network, select, or stat sources, so the probe
 * makes those calls through syscall(), and "resolve" does what a libc stub
 * resolver does: /etc/hosts, then an A query over UDP to the first
 * nameserver in /etc/resolv.conf, and again over TCP when the answer comes
 * back truncated.
 */
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <netinet/in.h>
#include <netinet/tcp.h>
#include <poll.h>
#include <signal.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/select.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <sys/time.h>
#include <sys/uio.h>
#include <sys/utsname.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

#define IMAGE "/system/linux/netprobe"
#define LOOPBACK 0x7f000001u
#define FOREIGN 0xcb007107u /* 203.0.113.7, owned by nobody */

static const char *self = IMAGE;
static const char *group = "netprobe";

/* GoOS names itself in the version field of uname. */
static int on_goos(void)
{
	struct utsname name;

	return uname(&name) == 0 && !strncmp(name.version, "GoOS ", 5);
}

static int failed(const char *step)
{
	printf("%s failed: %s errno=%d\n", group, step, errno);
	fflush(stdout);
	return 1;
}

/* trace, set by NETPROBE_TRACE in the environment, names each step on
 * stderr before it runs, so a step that never returns is visible. */
static int trace;

#define REQUIRE(condition, step) \
	do { \
		if (trace) \
			fprintf(stderr, "step %s\n", step); \
		if (!(condition)) \
			return failed(step); \
	} while (0)

/* fails says a call returned -1 with the given errno. */
static int fails(long result, int reason)
{
	return result == -1 && errno == reason;
}

static uint16_t swap16(uint16_t value)
{
	return (uint16_t)(value << 8 | value >> 8);
}

static uint32_t swap32(uint32_t value)
{
	return __builtin_bswap32(value);
}

/* --- system calls ------------------------------------------------------ */

static int sys_socket(int domain, int type, int protocol)
{
	return syscall(SYS_socket, domain, type, protocol);
}

static int sys_bind(int fd, const void *address, long length)
{
	return syscall(SYS_bind, fd, address, length);
}

static int sys_connect(int fd, const void *address, long length)
{
	return syscall(SYS_connect, fd, address, length);
}

static int sys_listen(int fd, int backlog)
{
	return syscall(SYS_listen, fd, backlog);
}

static int sys_accept4(int fd, void *address, socklen_t *length, int flags)
{
	return syscall(SYS_accept4, fd, address, length, flags);
}

static int sys_accept(int fd, void *address, socklen_t *length)
{
	return syscall(SYS_accept, fd, address, length);
}

static long sys_sendto(int fd, const void *data, size_t size, int flags, const void *address, long length)
{
	return syscall(SYS_sendto, fd, data, size, flags, address, length);
}

static long sys_recvfrom(int fd, void *data, size_t size, int flags, void *address, socklen_t *length)
{
	return syscall(SYS_recvfrom, fd, data, size, flags, address, length);
}

static long sys_send(int fd, const void *data, size_t size, int flags)
{
	return sys_sendto(fd, data, size, flags, NULL, 0);
}

static long sys_recv(int fd, void *data, size_t size, int flags)
{
	return sys_recvfrom(fd, data, size, flags, NULL, NULL);
}

static long sys_sendmsg(int fd, const struct msghdr *message, int flags)
{
	return syscall(SYS_sendmsg, fd, message, flags);
}

static long sys_recvmsg(int fd, struct msghdr *message, int flags)
{
	return syscall(SYS_recvmsg, fd, message, flags);
}

static int sys_shutdown(int fd, int how)
{
	return syscall(SYS_shutdown, fd, how);
}

static int sys_getsockname(int fd, void *address, socklen_t *length)
{
	return syscall(SYS_getsockname, fd, address, length);
}

static int sys_getpeername(int fd, void *address, socklen_t *length)
{
	return syscall(SYS_getpeername, fd, address, length);
}

static int sys_setsockopt(int fd, int level, int name, const void *value, long length)
{
	return syscall(SYS_setsockopt, fd, level, name, value, length);
}

static int sys_getsockopt(int fd, int level, int name, void *value, socklen_t *length)
{
	return syscall(SYS_getsockopt, fd, level, name, value, length);
}

static int sys_poll(struct pollfd *entries, unsigned long count, int timeout)
{
	return syscall(SYS_poll, entries, count, timeout);
}

static int sys_select(int count, fd_set *read, fd_set *write, fd_set *except, struct timeval *timeout)
{
	return syscall(SYS_select, count, read, write, except, timeout);
}

struct sigdata {
	const sigset_t *mask;
	size_t size;
};

static int sys_pselect6(int count, fd_set *read, fd_set *write, fd_set *except, struct timespec *timeout, const sigset_t *mask)
{
	struct sigdata data = {mask, 8};

	return syscall(SYS_pselect6, count, read, write, except, timeout, mask ? &data : NULL);
}

static int sys_fstat(int fd, struct stat *status)
{
	return syscall(SYS_fstat, fd, status);
}

/* --- helpers ----------------------------------------------------------- */

static struct sockaddr_in in4(uint32_t address, uint16_t port)
{
	struct sockaddr_in result;

	memset(&result, 0, sizeof(result));
	result.sin_family = AF_INET;
	result.sin_port = swap16(port);
	result.sin_addr.s_addr = swap32(address);
	return result;
}

static int bind4(int fd, uint32_t address, uint16_t port)
{
	struct sockaddr_in name = in4(address, port);

	return sys_bind(fd, &name, sizeof(name));
}

static int connect4(int fd, uint32_t address, uint16_t port)
{
	struct sockaddr_in name = in4(address, port);

	return sys_connect(fd, &name, sizeof(name));
}

/* local_port returns the port a socket is bound to, or -1. */
static int local_port(int fd)
{
	struct sockaddr_in name;
	socklen_t length = sizeof(name);

	if (sys_getsockname(fd, &name, &length) != 0 || length != sizeof(name))
		return -1;
	return swap16(name.sin_port);
}

static int same_address(const struct sockaddr_in *a, const struct sockaddr_in *b)
{
	return a->sin_family == b->sin_family && a->sin_port == b->sin_port && a->sin_addr.s_addr == b->sin_addr.s_addr;
}

static int int_option(int fd, int level, int name)
{
	int value = -12345;
	socklen_t length = sizeof(value);

	if (sys_getsockopt(fd, level, name, &value, &length) != 0 || length != sizeof(value))
		return -12345;
	return value;
}

static int set_int_option(int fd, int level, int name, int value)
{
	return sys_setsockopt(fd, level, name, &value, sizeof(value));
}

static void nap(int milliseconds)
{
	struct timespec time = {milliseconds / 1000, (long)(milliseconds % 1000) * 1000000L};

	while (nanosleep(&time, &time) != 0 && errno == EINTR)
		;
}

static long now_ms(void)
{
	struct timespec time;

	clock_gettime(CLOCK_MONOTONIC, &time);
	return time.tv_sec * 1000L + time.tv_nsec / 1000000L;
}

/* revents polls one descriptor and returns its revents, 0 on a timeout. */
static int revents(int fd, short events, int timeout)
{
	struct pollfd entry = {fd, events, 0};
	int result = sys_poll(&entry, 1, timeout);

	return result < 0 ? -1 : result == 0 ? 0 : entry.revents;
}

static int nonblocking(int fd, int on)
{
	int flags = fcntl(fd, F_GETFL);

	if (flags < 0)
		return -1;
	return fcntl(fd, F_SETFL, on ? flags | O_NONBLOCK : flags & ~O_NONBLOCK);
}

/* listener makes a loopback listener on an ephemeral port. */
static int listener(uint16_t *port, int type)
{
	int fd = sys_socket(AF_INET, SOCK_STREAM | type, 0);
	int bound;

	if (fd < 0)
		return -1;
	if (bind4(fd, LOOPBACK, 0) != 0 || sys_listen(fd, 8) != 0 || (bound = local_port(fd)) <= 0) {
		close(fd);
		return -1;
	}
	*port = (uint16_t)bound;
	return fd;
}

/* tcp_pair connects a client to a server over loopback. */
static int tcp_pair(int *client, int *server)
{
	uint16_t port;
	int fd = listener(&port, 0);

	*client = *server = -1;
	if (fd < 0)
		return -1;
	*client = sys_socket(AF_INET, SOCK_STREAM, 0);
	if (*client >= 0 && connect4(*client, LOOPBACK, port) == 0)
		*server = sys_accept4(fd, NULL, NULL, 0);
	close(fd);
	if (*server < 0) {
		if (*client >= 0)
			close(*client);
		*client = -1;
		return -1;
	}
	return 0;
}

static void close_pair(int client, int server)
{
	if (client >= 0)
		close(client);
	if (server >= 0)
		close(server);
}

/* closed_port returns a loopback port nothing listens on. */
static int closed_port(void)
{
	int fd = sys_socket(AF_INET, SOCK_STREAM, 0);
	int port;

	if (fd < 0 || bind4(fd, LOOPBACK, 0) != 0) {
		if (fd >= 0)
			close(fd);
		return -1;
	}
	port = local_port(fd);
	close(fd);
	return port;
}

/* read_full reads exactly size bytes, or fails. */
static int read_full(int fd, void *data, size_t size)
{
	size_t done = 0;

	while (done < size) {
		long n = read(fd, (char *)data + done, size - done);

		if (n < 0 && errno == EINTR)
			continue;
		if (n <= 0)
			return -1;
		done += (size_t)n;
	}
	return 0;
}

static int write_full(int fd, const void *data, size_t size)
{
	size_t done = 0;

	while (done < size) {
		long n = sys_send(fd, (const char *)data + done, size - done, MSG_NOSIGNAL);

		if (n < 0 && errno == EINTR)
			continue;
		if (trace)
			fprintf(stderr, "trace send %ld errno=%d done=%lu\n", n, n < 0 ? errno : 0, (unsigned long)done);
		if (n <= 0)
			return -1;
		done += (size_t)n;
	}
	return 0;
}

static pid_t spawn(void)
{
	fflush(stdout);
	return fork();
}

static int child_status(pid_t child)
{
	int status;

	while (waitpid(child, &status, 0) < 0)
		if (errno != EINTR)
			return -1;
	return status;
}

static unsigned char pattern_byte(size_t offset)
{
	return (unsigned char)(offset * 7 + offset / 251);
}

static volatile int signals_seen;

static void count_signal(int number)
{
	(void)number;
	signals_seen++;
}

static int handle(int number, void (*handler)(int), int flags)
{
	struct sigaction action;

	memset(&action, 0, sizeof(action));
	action.sa_handler = handler;
	action.sa_flags = flags;
	sigemptyset(&action.sa_mask);
	return sigaction(number, &action, NULL);
}

static const char *errno_name(int number)
{
	switch (number) {
	case 0: return "0";
	case EAGAIN: return "EAGAIN";
	case EINTR: return "EINTR";
	case EINVAL: return "EINVAL";
	case EPIPE: return "EPIPE";
	case ECONNREFUSED: return "ECONNREFUSED";
	case ECONNRESET: return "ECONNRESET";
	case ECONNABORTED: return "ECONNABORTED";
	case ETIMEDOUT: return "ETIMEDOUT";
	case EHOSTUNREACH: return "EHOSTUNREACH";
	case ENETUNREACH: return "ENETUNREACH";
	case ENETDOWN: return "ENETDOWN";
	case ENETRESET: return "ENETRESET";
	case ENOTCONN: return "ENOTCONN";
	case EADDRINUSE: return "EADDRINUSE";
	case EADDRNOTAVAIL: return "EADDRNOTAVAIL";
	case EMSGSIZE: return "EMSGSIZE";
	case EBADF: return "EBADF";
	case EIO: return "EIO";
	case ENOSYS: return "ENOSYS";
	case EMFILE: return "EMFILE";
	case ENOBUFS: return "ENOBUFS";
	case EAFNOSUPPORT: return "EAFNOSUPPORT";
	default: return "other";
	}
}

/* --- create ------------------------------------------------------------ */

static int create_group(void)
{
	int fd, flags;

	fd = sys_socket(AF_INET, SOCK_STREAM, 0);
	REQUIRE(fd >= 0, "stream");
	REQUIRE(fcntl(fd, F_GETFD) == 0, "stream not close-on-exec");
	flags = fcntl(fd, F_GETFL);
	REQUIRE(flags >= 0 && (flags & O_ACCMODE) == O_RDWR && !(flags & O_NONBLOCK), "stream status flags");
	REQUIRE(fcntl(fd, F_SETFL, flags | O_NONBLOCK) == 0 && (fcntl(fd, F_GETFL) & O_NONBLOCK), "F_SETFL O_NONBLOCK");
	REQUIRE(fcntl(fd, F_SETFD, FD_CLOEXEC) == 0 && fcntl(fd, F_GETFD) == FD_CLOEXEC, "F_SETFD");
	close(fd);
	fd = sys_socket(AF_INET, SOCK_STREAM, IPPROTO_TCP);
	REQUIRE(fd >= 0, "IPPROTO_TCP");
	close(fd);
	fd = sys_socket(AF_INET, SOCK_DGRAM, IPPROTO_UDP);
	REQUIRE(fd >= 0, "IPPROTO_UDP");
	close(fd);
	fd = sys_socket(AF_INET, SOCK_DGRAM | SOCK_CLOEXEC | SOCK_NONBLOCK, 0);
	REQUIRE(fd >= 0 && fcntl(fd, F_GETFD) == FD_CLOEXEC && (fcntl(fd, F_GETFL) & O_NONBLOCK), "SOCK_CLOEXEC|SOCK_NONBLOCK");
	close(fd);
	REQUIRE(fails(sys_socket(200, SOCK_STREAM, 0), EAFNOSUPPORT), "unknown family");
	REQUIRE(fails(sys_socket(AF_INET, SOCK_RDM, 0), ESOCKTNOSUPPORT), "SOCK_RDM");
	REQUIRE(fails(sys_socket(AF_INET, 12, 0), EINVAL), "type past SOCK_MAX");
	REQUIRE(fails(sys_socket(AF_INET, SOCK_STREAM | 0x10, 0), EINVAL), "unknown type flag");
	REQUIRE(fails(sys_socket(AF_INET, SOCK_STREAM, IPPROTO_UDP), EPROTONOSUPPORT), "stream with UDP");
	REQUIRE(fails(sys_socket(AF_INET, SOCK_DGRAM, IPPROTO_TCP), EPROTONOSUPPORT), "datagram with TCP");
	REQUIRE(fails(sys_socket(AF_INET, SOCK_STREAM, 253), EPROTONOSUPPORT), "unknown protocol");
	if (on_goos()) {
		/* Families and types Linux has and GoOS does not. */
		REQUIRE(fails(sys_socket(AF_INET6, SOCK_STREAM, 0), EAFNOSUPPORT), "AF_INET6");
		REQUIRE(fails(sys_socket(AF_UNIX, SOCK_STREAM, 0), EAFNOSUPPORT), "AF_UNIX");
		REQUIRE(fails(sys_socket(AF_NETLINK, SOCK_RAW, 0), EAFNOSUPPORT), "AF_NETLINK");
		REQUIRE(fails(sys_socket(AF_PACKET, SOCK_RAW, 0), EAFNOSUPPORT), "AF_PACKET");
		REQUIRE(sys_socket(AF_INET, SOCK_RAW, IPPROTO_ICMP) == -1, "SOCK_RAW");
		REQUIRE(sys_socket(AF_INET, SOCK_SEQPACKET, 0) == -1, "SOCK_SEQPACKET");
	}
	printf("create ok\n");
	return 0;
}

/* --- sockaddr ---------------------------------------------------------- */

static int sockaddr_group(void)
{
	union {
		struct sockaddr_in in;
		unsigned char bytes[256];
	} name;
	struct sockaddr_in loop = in4(LOOPBACK, 0), wrong;
	socklen_t length;
	int fd, other, udp, client, server, port, i;

	fd = sys_socket(AF_INET, SOCK_STREAM, 0);
	REQUIRE(fd >= 0, "socket");
	REQUIRE(fails(sys_bind(fd, &loop, sizeof(loop) - 1), EINVAL), "bind with 15 bytes");
	REQUIRE(fails(sys_bind(fd, &loop, 0), EINVAL), "bind with 0 bytes");
	memset(&name, 0, sizeof(name));
	name.in = loop;
	REQUIRE(fails(sys_bind(fd, &name, 129), EINVAL), "bind with 129 bytes");
	wrong = loop;
	wrong.sin_family = AF_INET6;
	REQUIRE(fails(sys_bind(fd, &wrong, sizeof(wrong)), EAFNOSUPPORT), "bind with another family");
	REQUIRE(sys_bind(fd, &name, 32) == 0, "bind with 32 bytes");
	port = local_port(fd);
	REQUIRE(port > 0, "bound port");

	memset(&name, 0xaa, sizeof(name));
	length = 4;
	REQUIRE(sys_getsockname(fd, &name, &length) == 0 && length == 16, "getsockname truncated length");
	REQUIRE(name.in.sin_family == AF_INET && swap16(name.in.sin_port) == port, "getsockname truncated prefix");
	for (i = 4; i < 32; i++)
		REQUIRE(name.bytes[i] == 0xaa, "getsockname wrote past 4 bytes");
	memset(&name, 0xaa, sizeof(name));
	length = 32;
	REQUIRE(sys_getsockname(fd, &name, &length) == 0 && length == 16, "getsockname oversized length");
	REQUIRE(name.in.sin_addr.s_addr == swap32(LOOPBACK), "getsockname address");
	for (i = 8; i < 16; i++)
		REQUIRE(name.bytes[i] == 0, "sin_zero not zero");
	for (i = 16; i < 32; i++)
		REQUIRE(name.bytes[i] == 0xaa, "getsockname wrote past 16 bytes");
	length = (socklen_t)-1;
	REQUIRE(fails(sys_getsockname(fd, &name, &length), EINVAL), "getsockname negative length");
	length = sizeof(name);
	REQUIRE(fails(sys_getpeername(fd, &name, &length), ENOTCONN), "getpeername unconnected");
	close(fd);

	other = sys_socket(AF_INET, SOCK_STREAM, 0);
	REQUIRE(other >= 0, "connect socket");
	REQUIRE(fails(sys_connect(other, &loop, sizeof(loop) - 1), EINVAL), "connect with 15 bytes");
	close(other);
	udp = sys_socket(AF_INET, SOCK_DGRAM, 0);
	REQUIRE(udp >= 0, "udp socket");
	REQUIRE(fails(sys_sendto(udp, "x", 1, 0, &loop, sizeof(loop) - 1), EINVAL), "sendto with 15 bytes");
	REQUIRE(fails(sys_connect(udp, &loop, sizeof(loop) - 1), EINVAL), "udp connect with 15 bytes");
	close(udp);

	REQUIRE(tcp_pair(&client, &server) == 0, "pair");
	memset(&name, 0xaa, sizeof(name));
	length = 8;
	REQUIRE(sys_getpeername(client, &name, &length) == 0 && length == 16, "getpeername truncated length");
	for (i = 8; i < 16; i++)
		REQUIRE(name.bytes[i] == 0xaa, "getpeername wrote past 8 bytes");
	{
		struct sockaddr_in peer, local;
		socklen_t a = sizeof(peer), b = sizeof(local);

		REQUIRE(sys_getpeername(server, &peer, &a) == 0 && sys_getsockname(client, &local, &b) == 0 &&
		        a == 16 && b == 16 && same_address(&peer, &local), "server's peer is the client");
	}
	close_pair(client, server);
	printf("sockaddr ok\n");
	return 0;
}

/* --- bind -------------------------------------------------------------- */

static int bind_group(void)
{
	int first, second, wild, foreign, other, udp, reuse, plain, client, accepted, fresh, again;
	uint16_t port;
	char byte;

	first = sys_socket(AF_INET, SOCK_STREAM, 0);
	REQUIRE(first >= 0 && bind4(first, LOOPBACK, 0) == 0, "bind port 0");
	REQUIRE(local_port(first) > 0, "ephemeral port");
	port = (uint16_t)local_port(first);
	REQUIRE(fails(bind4(first, LOOPBACK, 0), EINVAL), "second bind of one socket");
	second = sys_socket(AF_INET, SOCK_STREAM, 0);
	REQUIRE(second >= 0 && fails(bind4(second, LOOPBACK, port), EADDRINUSE), "same address and port");
	wild = sys_socket(AF_INET, SOCK_STREAM, 0);
	REQUIRE(wild >= 0 && fails(bind4(wild, INADDR_ANY, port), EADDRINUSE), "wildcard over a bound port");
	udp = sys_socket(AF_INET, SOCK_DGRAM, 0);
	REQUIRE(udp >= 0 && bind4(udp, LOOPBACK, port) == 0, "UDP on a TCP port");
	foreign = sys_socket(AF_INET, SOCK_STREAM, 0);
	REQUIRE(foreign >= 0 && fails(bind4(foreign, FOREIGN, 0), EADDRNOTAVAIL), "foreign address");
	other = sys_socket(AF_INET, SOCK_STREAM, 0);
	REQUIRE(other >= 0 && bind4(other, 0x7f000002u, 0) == 0, "127.0.0.2");
	close(first);
	close(second);
	close(wild);
	close(udp);
	close(foreign);
	close(other);

	/* A server restarts on its port while a connection it closed first
	 * waits in TIME_WAIT: only SO_REUSEADDR may bind again. */
	reuse = sys_socket(AF_INET, SOCK_STREAM, 0);
	REQUIRE(reuse >= 0 && set_int_option(reuse, SOL_SOCKET, SO_REUSEADDR, 1) == 0, "SO_REUSEADDR");
	REQUIRE(bind4(reuse, LOOPBACK, 0) == 0 && sys_listen(reuse, 4) == 0, "reusable listener");
	port = (uint16_t)local_port(reuse);
	client = sys_socket(AF_INET, SOCK_STREAM, 0);
	REQUIRE(client >= 0 && connect4(client, LOOPBACK, port) == 0, "connect to the listener");
	accepted = sys_accept4(reuse, NULL, NULL, 0);
	REQUIRE(accepted >= 0, "accept");
	close(accepted);
	REQUIRE(read(client, &byte, 1) == 0, "client sees the server close");
	close(client);
	close(reuse);
	nap(200);
	plain = sys_socket(AF_INET, SOCK_STREAM, 0);
	REQUIRE(plain >= 0 && fails(bind4(plain, LOOPBACK, port), EADDRINUSE), "rebind without SO_REUSEADDR");
	close(plain);
	fresh = sys_socket(AF_INET, SOCK_STREAM, 0);
	REQUIRE(fresh >= 0 && set_int_option(fresh, SOL_SOCKET, SO_REUSEADDR, 1) == 0, "SO_REUSEADDR again");
	REQUIRE(bind4(fresh, LOOPBACK, port) == 0, "rebind with SO_REUSEADDR");
	REQUIRE(sys_listen(fresh, 4) == 0, "listen after rebind");
	client = sys_socket(AF_INET, SOCK_STREAM, 0);
	REQUIRE(client >= 0 && connect4(client, LOOPBACK, port) == 0, "connect to the restarted server");
	again = sys_accept4(fresh, NULL, NULL, 0);
	REQUIRE(again >= 0, "accept on the restarted server");
	close(again);
	close(client);
	close(fresh);
	printf("bind ok\n");
	return 0;
}

/* --- listen ------------------------------------------------------------ */

static int listen_group(void)
{
	struct sockaddr_in peer, local;
	socklen_t length, local_length;
	int fd, idle, udp, client, accepted, early, flags;
	uint16_t port;
	char byte;

	fd = sys_socket(AF_INET, SOCK_STREAM | SOCK_NONBLOCK, 0);
	REQUIRE(fd >= 0 && bind4(fd, LOOPBACK, 0) == 0, "bind");
	REQUIRE(int_option(fd, SOL_SOCKET, SO_ACCEPTCONN) == 0, "SO_ACCEPTCONN before listen");
	REQUIRE(fails(sys_accept4(fd, NULL, NULL, 0), EINVAL), "accept before listen");
	REQUIRE(sys_listen(fd, 8) == 0, "listen");
	REQUIRE(int_option(fd, SOL_SOCKET, SO_ACCEPTCONN) == 1, "SO_ACCEPTCONN after listen");
	REQUIRE(sys_listen(fd, 1000) == 0, "listen again with a larger backlog");
	port = (uint16_t)local_port(fd);
	REQUIRE(fails(sys_accept4(fd, NULL, NULL, 0), EAGAIN), "nonblocking accept with nothing pending");
	REQUIRE(revents(fd, POLLIN, 0) == 0, "idle listener not readable");
	REQUIRE(fails(sys_accept4(fd, NULL, NULL, 0x1), EINVAL), "accept4 with an unknown flag");
	udp = sys_socket(AF_INET, SOCK_DGRAM, 0);
	REQUIRE(udp >= 0 && fails(sys_listen(udp, 1), EOPNOTSUPP), "listen on UDP");
	close(udp);
	idle = sys_socket(AF_INET, SOCK_STREAM, 0);
	REQUIRE(idle >= 0 && fails(sys_accept4(idle, NULL, NULL, 0), EINVAL), "accept on an unbound socket");
	close(idle);

	client = sys_socket(AF_INET, SOCK_STREAM, 0);
	REQUIRE(client >= 0 && connect4(client, LOOPBACK, port) == 0, "connect");
	REQUIRE(revents(fd, POLLIN, 2000) == POLLIN, "listener readable with a connection pending");
	accepted = sys_accept(fd, NULL, NULL);
	REQUIRE(accepted >= 0, "accept");
	flags = fcntl(accepted, F_GETFL);
	REQUIRE(flags >= 0 && !(flags & O_NONBLOCK) && fcntl(accepted, F_GETFD) == 0, "accept inherits no flags");
	close(accepted);
	close(client);

	client = sys_socket(AF_INET, SOCK_STREAM, 0);
	REQUIRE(client >= 0 && connect4(client, LOOPBACK, port) == 0, "connect for accept4");
	REQUIRE(revents(fd, POLLIN, 2000) == POLLIN, "pending for accept4");
	length = sizeof(peer);
	accepted = sys_accept4(fd, &peer, &length, SOCK_CLOEXEC | SOCK_NONBLOCK);
	REQUIRE(accepted >= 0, "accept4");
	REQUIRE(fcntl(accepted, F_GETFD) == FD_CLOEXEC && (fcntl(accepted, F_GETFL) & O_NONBLOCK), "accept4 flags");
	local_length = sizeof(local);
	REQUIRE(length == 16 && sys_getsockname(client, &local, &local_length) == 0 && same_address(&peer, &local), "accept4 peer address");
	REQUIRE(fails(read(accepted, &byte, 1), EAGAIN), "accepted socket is nonblocking");
	close(accepted);
	close(client);

	client = sys_socket(AF_INET, SOCK_STREAM, 0);
	REQUIRE(client >= 0 && connect4(client, LOOPBACK, port) == 0, "connect for a short name");
	REQUIRE(revents(fd, POLLIN, 2000) == POLLIN, "pending for a short name");
	memset(&peer, 0xaa, sizeof(peer));
	length = 4;
	accepted = sys_accept4(fd, &peer, &length, 0);
	REQUIRE(accepted >= 0 && length == 16 && peer.sin_family == AF_INET && peer.sin_addr.s_addr == 0xaaaaaaaau, "accept truncates the name");
	close(accepted);
	close(client);

	/* A client that goes away before accept still leaves a connection,
	 * which reads end of file. */
	early = sys_socket(AF_INET, SOCK_STREAM, 0);
	REQUIRE(early >= 0 && connect4(early, LOOPBACK, port) == 0, "connect and leave");
	close(early);
	REQUIRE(revents(fd, POLLIN, 2000) == POLLIN, "pending after the client left");
	accepted = sys_accept4(fd, NULL, NULL, 0);
	REQUIRE(accepted >= 0, "accept after the client left");
	REQUIRE(revents(accepted, POLLIN, 2000) & POLLIN, "end of file readable");
	REQUIRE(read(accepted, &byte, 1) == 0, "end of file");
	close(accepted);
	close(fd);
	printf("listen ok\n");
	return 0;
}

/* --- connect ----------------------------------------------------------- */

static int connect_group(void)
{
	struct sockaddr_in name;
	socklen_t length;
	int fd, client, accepted, port, events;
	uint16_t listening;

	fd = listener(&listening, 0);
	REQUIRE(fd >= 0, "listener");
	client = sys_socket(AF_INET, SOCK_STREAM | SOCK_NONBLOCK, 0);
	REQUIRE(client >= 0, "socket");
	REQUIRE(fails(connect4(client, LOOPBACK, listening), EINPROGRESS), "nonblocking connect in progress");
	events = revents(client, POLLOUT, 2000);
	REQUIRE(events > 0 && (events & POLLOUT) && !(events & (POLLERR | POLLHUP)), "writable once connected");
	REQUIRE(int_option(client, SOL_SOCKET, SO_ERROR) == 0, "SO_ERROR after success");
	/* Linux lets the next connect report the finished attempt; after
	 * that the socket is connected. */
	REQUIRE(connect4(client, LOOPBACK, listening) == 0 || errno == EISCONN, "connect after completion");
	REQUIRE(fails(connect4(client, LOOPBACK, listening), EISCONN), "connect again");
	accepted = sys_accept4(fd, NULL, NULL, 0);
	REQUIRE(accepted >= 0, "accept");
	close(accepted);
	close(client);
	close(fd);

	port = closed_port();
	REQUIRE(port > 0, "closed port");
	client = sys_socket(AF_INET, SOCK_STREAM, 0);
	REQUIRE(client >= 0 && fails(connect4(client, LOOPBACK, (uint16_t)port), ECONNREFUSED), "blocking connect refused");
	close(client);
	client = sys_socket(AF_INET, SOCK_STREAM | SOCK_NONBLOCK, 0);
	REQUIRE(client >= 0 && fails(connect4(client, LOOPBACK, (uint16_t)port), EINPROGRESS), "nonblocking connect to a closed port");
	events = revents(client, POLLOUT, 2000);
	REQUIRE(events > 0 && (events & POLLOUT) && (events & POLLERR), "refusal is POLLOUT and POLLERR");
	REQUIRE(int_option(client, SOL_SOCKET, SO_ERROR) == ECONNREFUSED, "SO_ERROR names the refusal");
	REQUIRE(int_option(client, SOL_SOCKET, SO_ERROR) == 0, "SO_ERROR reads once");
	length = sizeof(name);
	REQUIRE(fails(sys_getpeername(client, &name, &length), ENOTCONN), "no peer after refusal");
	close(client);
	printf("connect ok\n");
	return 0;
}

/* --- stream ------------------------------------------------------------ */

#define LARGE_TRANSFER 300000
#define NONBLOCK_CAP (64L << 20)

static unsigned long long sum_bytes(const unsigned char *data, size_t size, unsigned long long sum)
{
	size_t i;

	for (i = 0; i < size; i++)
		sum = sum * 1099511628211ULL ^ data[i];
	return sum;
}

static int stream_group(void)
{
	static unsigned char chunk[65536];
	char buffer[64];
	struct iovec pieces[3] = {{"ab", 2}, {"cd", 2}, {"ef", 2}}, halves[2];
	char left[3], right[3];
	unsigned long long sum = 1469598103934665603ULL, echoed;
	long total, got, n;
	int client, server, queued, status, events;
	size_t i, offset;
	pid_t child;

	REQUIRE(tcp_pair(&client, &server) == 0, "pair");
	REQUIRE(write(client, "hello", 5) == 5, "write");
	REQUIRE(revents(server, POLLIN, 2000) & POLLIN, "readable");
	REQUIRE(ioctl(server, FIONREAD, &queued) == 0 && queued == 5, "FIONREAD counts buffered bytes");
	REQUIRE(sys_recv(server, buffer, sizeof(buffer), MSG_PEEK) == 5 && !memcmp(buffer, "hello", 5), "MSG_PEEK");
	REQUIRE(ioctl(server, FIONREAD, &queued) == 0 && queued == 5, "MSG_PEEK consumes nothing");
	REQUIRE(read(server, buffer, sizeof(buffer)) == 5 && !memcmp(buffer, "hello", 5), "read after peek");
	REQUIRE(fails(sys_recv(server, buffer, sizeof(buffer), MSG_DONTWAIT), EAGAIN), "MSG_DONTWAIT");
	REQUIRE(ioctl(server, FIONREAD, &queued) == 0 && queued == 0, "FIONREAD empty");
	REQUIRE(writev(client, pieces, 3) == 6, "writev");
	REQUIRE(revents(server, POLLIN, 2000) & POLLIN, "readable after writev");
	nap(50);
	halves[0] = (struct iovec){left, 3};
	halves[1] = (struct iovec){right, 3};
	REQUIRE(readv(server, halves, 2) == 6 && !memcmp(left, "abc", 3) && !memcmp(right, "def", 3), "readv");

	/* A blocking write larger than every buffer finishes while a child
	 * reads. */
	child = spawn();
	if (child == 0) {
		unsigned long long seen = 1469598103934665603ULL;
		long left_bytes = LARGE_TRANSFER;

		while (left_bytes > 0) {
			n = read(server, chunk, left_bytes < (long)sizeof(chunk) ? left_bytes : (long)sizeof(chunk));
			if (trace)
				fprintf(stderr, "trace read %ld errno=%d left=%ld\n", n, n < 0 ? errno : 0, left_bytes);
			if (n <= 0)
				_exit(2);
			seen = sum_bytes(chunk, (size_t)n, seen);
			left_bytes -= n;
		}
		_exit(write_full(server, &seen, sizeof(seen)) == 0 ? 0 : 3);
	}
	REQUIRE(child > 0, "fork reader");
	{
		static unsigned char large[LARGE_TRANSFER];

		for (i = 0; i < LARGE_TRANSFER; i++)
			large[i] = pattern_byte(i);
		sum = sum_bytes(large, LARGE_TRANSFER, sum);
		REQUIRE(write_full(client, large, LARGE_TRANSFER) == 0, "blocking write of 300000 bytes");
	}
	REQUIRE(read_full(client, &echoed, sizeof(echoed)) == 0 && echoed == sum, "reader saw every byte");
	status = child_status(child);
	REQUIRE(WIFEXITED(status) && WEXITSTATUS(status) == 0, "reader exit");

	/* MSG_WAITALL waits for the second write. */
	child = spawn();
	if (child == 0) {
		write(client, "abc", 3);
		nap(100);
		write(client, "defgh", 5);
		_exit(0);
	}
	REQUIRE(child > 0, "fork writer");
	REQUIRE(sys_recv(server, buffer, 8, MSG_WAITALL) == 8 && !memcmp(buffer, "abcdefgh", 8), "MSG_WAITALL");
	child_status(child);

	/* Nonblocking writes take what fits until EAGAIN; every byte then
	 * arrives in order. */
	REQUIRE(nonblocking(client, 1) == 0, "O_NONBLOCK");
	total = 0;
	for (;;) {
		for (i = 0; i < sizeof(chunk); i++)
			chunk[i] = pattern_byte((size_t)total + i);
		n = write(client, chunk, sizeof(chunk));
		if (n < 0) {
			REQUIRE(errno == EAGAIN, "nonblocking write error");
			break;
		}
		REQUIRE(n > 0, "nonblocking write of zero bytes");
		total += n;
		REQUIRE(total < NONBLOCK_CAP, "nonblocking writes never filled the buffer");
	}
	REQUIRE(total > 0, "nothing written before EAGAIN");
	for (got = 0; got < total;) {
		n = read(server, chunk, sizeof(chunk));
		REQUIRE(n > 0, "drain");
		for (offset = 0; offset < (size_t)n; offset++)
			REQUIRE(chunk[offset] == pattern_byte((size_t)got + offset), "drained bytes in order");
		got += n;
	}
	REQUIRE(got == total, "drained count");
	events = revents(client, POLLOUT, 2000);
	REQUIRE(events > 0 && (events & POLLOUT), "writable after the drain");
	close_pair(client, server);

	/* Buffered bytes come before end of file. */
	REQUIRE(tcp_pair(&client, &server) == 0, "pair for end of file");
	REQUIRE(write(client, "tail", 4) == 4, "write before close");
	close(client);
	events = revents(server, POLLRDHUP, 2000);
	REQUIRE(events > 0 && (events & POLLRDHUP), "POLLRDHUP after the peer closed");
	events = revents(server, POLLIN | POLLRDHUP, 0);
	REQUIRE((events & POLLIN) && !(events & POLLHUP), "readable, not hung up");
	REQUIRE(read(server, buffer, sizeof(buffer)) == 4 && !memcmp(buffer, "tail", 4), "buffered bytes first");
	REQUIRE(read(server, buffer, sizeof(buffer)) == 0 && read(server, buffer, sizeof(buffer)) == 0, "then end of file");
	REQUIRE(ioctl(server, FIONREAD, &queued) == 0 && queued == 0, "FIONREAD at end of file");
	close(server);
	printf("stream ok\n");
	return 0;
}

/* --- shutdown ---------------------------------------------------------- */

static int shutdown_group(void)
{
	char buffer[16];
	int fd, client, server, events;

	fd = sys_socket(AF_INET, SOCK_STREAM, 0);
	REQUIRE(fd >= 0 && fails(sys_shutdown(fd, SHUT_RDWR), ENOTCONN), "shutdown unconnected");
	close(fd);

	REQUIRE(tcp_pair(&client, &server) == 0, "pair");
	REQUIRE(fails(sys_shutdown(client, 3), EINVAL), "shutdown how=3");
	REQUIRE(fails(sys_shutdown(client, -1), EINVAL), "shutdown how=-1");
	REQUIRE(sys_shutdown(client, SHUT_WR) == 0 && sys_shutdown(client, SHUT_WR) == 0, "SHUT_WR twice");
	REQUIRE(revents(server, POLLIN, 2000) & POLLIN, "half-close readable");
	REQUIRE(read(server, buffer, sizeof(buffer)) == 0, "half-close is end of file");
	REQUIRE(write(server, "back", 4) == 4, "write toward the half-closed side");
	REQUIRE(revents(client, POLLIN, 2000) & POLLIN, "half-closed side readable");
	REQUIRE(read(client, buffer, sizeof(buffer)) == 4 && !memcmp(buffer, "back", 4), "half-closed side reads");
	REQUIRE(fails(sys_send(client, "x", 1, MSG_NOSIGNAL), EPIPE), "write after SHUT_WR");
	close_pair(client, server);

	REQUIRE(tcp_pair(&client, &server) == 0, "pair for SHUT_RD");
	REQUIRE(nonblocking(client, 1) == 0, "O_NONBLOCK");
	REQUIRE(sys_shutdown(client, SHUT_RD) == 0, "SHUT_RD");
	REQUIRE(read(client, buffer, sizeof(buffer)) == 0, "read after SHUT_RD is end of file");
	REQUIRE(revents(client, POLLIN, 0) & POLLIN, "readable after SHUT_RD");
	REQUIRE(write(client, "still", 5) == 5, "write side open after SHUT_RD");
	REQUIRE(revents(server, POLLIN, 2000) & POLLIN, "peer readable");
	REQUIRE(read(server, buffer, sizeof(buffer)) == 5 && !memcmp(buffer, "still", 5), "peer reads");
	close_pair(client, server);

	REQUIRE(tcp_pair(&client, &server) == 0, "pair for SHUT_RDWR");
	REQUIRE(sys_shutdown(client, SHUT_RDWR) == 0, "SHUT_RDWR");
	events = revents(client, POLLIN | POLLOUT, 2000);
	REQUIRE(events > 0 && (events & POLLHUP), "POLLHUP after SHUT_RDWR");
	REQUIRE(revents(server, POLLIN, 2000) & POLLIN, "peer sees the close");
	REQUIRE(read(server, buffer, sizeof(buffer)) == 0, "peer end of file");
	close_pair(client, server);
	printf("shutdown ok\n");
	return 0;
}

/* --- sigpipe ----------------------------------------------------------- */

static int sigpipe_group(void)
{
	sigset_t block, pending;
	int client, server, status;
	pid_t child;

	REQUIRE(tcp_pair(&client, &server) == 0, "pair");
	REQUIRE(sys_shutdown(client, SHUT_WR) == 0, "SHUT_WR");
	signals_seen = 0;
	REQUIRE(handle(SIGPIPE, count_signal, 0) == 0, "handler");
	REQUIRE(fails(write(client, "x", 1), EPIPE) && signals_seen == 1, "handled SIGPIPE");
	REQUIRE(fails(sys_send(client, "x", 1, MSG_NOSIGNAL), EPIPE) && signals_seen == 1, "MSG_NOSIGNAL");
	sigemptyset(&block);
	sigaddset(&block, SIGPIPE);
	REQUIRE(sigprocmask(SIG_BLOCK, &block, NULL) == 0, "block");
	REQUIRE(fails(write(client, "x", 1), EPIPE) && signals_seen == 1, "blocked SIGPIPE");
	REQUIRE(sigpending(&pending) == 0 && sigismember(&pending, SIGPIPE), "SIGPIPE pending");
	signal(SIGPIPE, SIG_IGN);
	REQUIRE(sigpending(&pending) == 0 && !sigismember(&pending, SIGPIPE), "SIG_IGN discards the pending SIGPIPE");
	REQUIRE(sigprocmask(SIG_UNBLOCK, &block, NULL) == 0 && signals_seen == 1, "unblock");
	REQUIRE(fails(write(client, "x", 1), EPIPE), "ignored SIGPIPE");
	child = spawn();
	if (child == 0) {
		signal(SIGPIPE, SIG_DFL);
		write(client, "x", 1);
		_exit(0);
	}
	REQUIRE(child > 0, "fork");
	status = child_status(child);
	REQUIRE(WIFSIGNALED(status) && WTERMSIG(status) == SIGPIPE, "default SIGPIPE ends the process");
	close_pair(client, server);
	printf("sigpipe ok\n");
	return 0;
}

/* --- poll -------------------------------------------------------------- */

static int poll_group(void)
{
	char buffer[16];
	int fd, udp, server, client, other, ghost, events, all = POLLIN | POLLOUT | POLLRDHUP;
	uint16_t port;

	fd = sys_socket(AF_INET, SOCK_STREAM, 0);
	REQUIRE(fd >= 0 && revents(fd, POLLIN | POLLOUT, 0) == (POLLOUT | POLLHUP), "unconnected stream");
	close(fd);
	udp = sys_socket(AF_INET, SOCK_DGRAM, 0);
	REQUIRE(udp >= 0 && revents(udp, POLLIN | POLLOUT, 0) == POLLOUT, "datagram socket");
	close(udp);

	fd = listener(&port, 0);
	REQUIRE(fd >= 0 && revents(fd, POLLIN | POLLOUT, 0) == 0, "idle listener");
	client = sys_socket(AF_INET, SOCK_STREAM, 0);
	REQUIRE(client >= 0 && connect4(client, LOOPBACK, port) == 0, "connect");
	REQUIRE(revents(fd, POLLIN | POLLOUT, 2000) == POLLIN, "listener with a connection");
	server = sys_accept4(fd, NULL, NULL, 0);
	REQUIRE(server >= 0, "accept");
	close(fd);
	REQUIRE(revents(server, all, 0) == POLLOUT, "idle connection");
	REQUIRE(write(client, "x", 1) == 1, "write");
	REQUIRE(revents(server, POLLIN, 2000) == POLLIN, "data arrives");
	REQUIRE(revents(server, all, 0) == (POLLIN | POLLOUT), "data and room");
	REQUIRE(read(server, buffer, sizeof(buffer)) == 1, "read");
	close(client);
	events = revents(server, POLLRDHUP, 2000);
	REQUIRE(events > 0 && (events & POLLRDHUP), "peer closed");
	REQUIRE(revents(server, all, 0) == all, "peer closed: in, out, read hangup");
	REQUIRE(sys_shutdown(server, SHUT_WR) == 0, "SHUT_WR");
	events = revents(server, POLLIN, 2000);
	REQUIRE(events > 0 && (events & POLLHUP) && (events & POLLIN), "both directions closed");
	other = dup(server);
	REQUIRE(other >= 0, "dup");
	close(other);
	ghost = other;
	REQUIRE(revents(ghost, POLLIN, 0) == POLLNVAL, "POLLNVAL for a closed descriptor");
	close(server);
	printf("poll ok\n");
	return 0;
}

/* --- select ------------------------------------------------------------ */

static int select_group(void)
{
	fd_set read_set, write_set;
	struct timeval wait;
	struct timespec spec;
	sigset_t block, empty, current;
	int fd, waiting, busy_client, busy_server, idle_client, idle_server, highest, result;
	uint16_t port;
	long start;

	fd = listener(&port, 0);
	REQUIRE(fd >= 0, "listener");
	waiting = sys_socket(AF_INET, SOCK_STREAM, 0);
	REQUIRE(waiting >= 0 && connect4(waiting, LOOPBACK, port) == 0, "connect");
	REQUIRE(revents(fd, POLLIN, 2000) == POLLIN, "pending");
	REQUIRE(tcp_pair(&busy_client, &busy_server) == 0 && tcp_pair(&idle_client, &idle_server) == 0, "pairs");
	REQUIRE(write(busy_client, "x", 1) == 1 && (revents(busy_server, POLLIN, 2000) & POLLIN), "data");
	highest = fd;
	if (busy_server > highest)
		highest = busy_server;
	if (idle_server > highest)
		highest = idle_server;
	if (idle_client > highest)
		highest = idle_client;

	FD_ZERO(&read_set);
	FD_ZERO(&write_set);
	FD_SET(fd, &read_set);
	FD_SET(busy_server, &read_set);
	FD_SET(idle_server, &read_set);
	FD_SET(idle_client, &write_set);
	wait = (struct timeval){1, 0};
	result = sys_select(highest + 1, &read_set, &write_set, NULL, &wait);
	REQUIRE(result == 3, "select count");
	REQUIRE(FD_ISSET(fd, &read_set) && FD_ISSET(busy_server, &read_set) && !FD_ISSET(idle_server, &read_set), "select read set");
	REQUIRE(FD_ISSET(idle_client, &write_set), "select write set");

	FD_ZERO(&read_set);
	FD_SET(idle_server, &read_set);
	wait = (struct timeval){0, 50000};
	start = now_ms();
	REQUIRE(sys_select(idle_server + 1, &read_set, NULL, NULL, &wait) == 0, "select timeout");
	REQUIRE(now_ms() - start >= 40 && !FD_ISSET(idle_server, &read_set), "select waited");
	REQUIRE(wait.tv_sec == 0 && wait.tv_usec == 0, "select writes the time left");
	wait = (struct timeval){-1, 0};
	REQUIRE(fails(sys_select(idle_server + 1, &read_set, NULL, NULL, &wait), EINVAL), "negative timeout");
	REQUIRE(fails(sys_select(-1, NULL, NULL, NULL, NULL), EINVAL), "negative count");

	FD_ZERO(&read_set);
	FD_ZERO(&write_set);
	FD_SET(fd, &read_set);
	FD_SET(busy_server, &read_set);
	FD_SET(idle_server, &read_set);
	FD_SET(idle_client, &write_set);
	spec = (struct timespec){1, 0};
	REQUIRE(sys_pselect6(highest + 1, &read_set, &write_set, NULL, &spec, NULL) == 3, "pselect6 count");
	REQUIRE(FD_ISSET(fd, &read_set) && FD_ISSET(busy_server, &read_set) && !FD_ISSET(idle_server, &read_set) &&
	        FD_ISSET(idle_client, &write_set), "pselect6 sets");
	FD_ZERO(&read_set);
	FD_SET(idle_server, &read_set);
	spec = (struct timespec){0, 50000000};
	start = now_ms();
	REQUIRE(sys_pselect6(idle_server + 1, &read_set, NULL, NULL, &spec, NULL) == 0 && now_ms() - start >= 40, "pselect6 timeout");
	/* Linux writes the time left back through the raw call; GoOS leaves
	 * the timespec alone. Both are a finished timeout. */
	REQUIRE(spec.tv_sec == 0 && (spec.tv_nsec == 0 || spec.tv_nsec == 50000000), "pselect6 timespec");

	/* A temporary mask lets a pending signal interrupt pselect6, and the
	 * old mask is back when it returns. */
	signals_seen = 0;
	REQUIRE(handle(SIGUSR1, count_signal, 0) == 0, "handler");
	sigemptyset(&block);
	sigaddset(&block, SIGUSR1);
	sigemptyset(&empty);
	REQUIRE(sigprocmask(SIG_BLOCK, &block, NULL) == 0 && raise(SIGUSR1) == 0 && signals_seen == 0, "pending SIGUSR1");
	FD_ZERO(&read_set);
	FD_SET(idle_server, &read_set);
	spec = (struct timespec){2, 0};
	REQUIRE(fails(sys_pselect6(idle_server + 1, &read_set, NULL, NULL, &spec, &empty), EINTR), "pselect6 interrupted");
	REQUIRE(signals_seen == 1, "handler ran");
	REQUIRE(sigprocmask(SIG_SETMASK, NULL, &current) == 0 && sigismember(&current, SIGUSR1), "mask restored");
	REQUIRE(sigprocmask(SIG_UNBLOCK, &block, NULL) == 0, "unblock");
	signal(SIGUSR1, SIG_DFL);

	close(waiting);
	close(fd);
	close_pair(busy_client, busy_server);
	close_pair(idle_client, idle_server);
	printf("select ok\n");
	return 0;
}

/* --- udp --------------------------------------------------------------- */

static int wait_datagram(int fd)
{
	return (revents(fd, POLLIN, 2000) & POLLIN) ? 0 : -1;
}

static int udp_group(void)
{
	static char big[65508];
	char buffer[128], first[2], second[16];
	struct sockaddr_in from, target, peer;
	struct iovec pieces[2];
	struct msghdr message;
	char control[64];
	socklen_t length;
	int receiver, sender, connected, stranger, queued, port, sender_port, i;
	long n;

	receiver = sys_socket(AF_INET, SOCK_DGRAM, 0);
	REQUIRE(receiver >= 0 && bind4(receiver, LOOPBACK, 0) == 0, "bind receiver");
	port = local_port(receiver);
	target = in4(LOOPBACK, (uint16_t)port);
	sender = sys_socket(AF_INET, SOCK_DGRAM, 0);
	REQUIRE(sender >= 0 && local_port(sender) == 0, "sender unbound");
	REQUIRE(fails(sys_send(sender, "x", 1, 0), EDESTADDRREQ), "send without an address");
	length = sizeof(peer);
	REQUIRE(fails(sys_getpeername(sender, &peer, &length), ENOTCONN), "getpeername unconnected");
	REQUIRE(sys_sendto(sender, "one", 3, 0, &target, sizeof(target)) == 3, "sendto");
	sender_port = local_port(sender);
	REQUIRE(sender_port > 0, "sendto binds a port");
	REQUIRE(wait_datagram(receiver) == 0, "datagram arrives");
	REQUIRE(ioctl(receiver, FIONREAD, &queued) == 0 && queued == 3, "FIONREAD");
	length = sizeof(from);
	REQUIRE(sys_recvfrom(receiver, buffer, sizeof(buffer), 0, &from, &length) == 3 && !memcmp(buffer, "one", 3), "recvfrom");
	REQUIRE(length == 16 && from.sin_family == AF_INET && from.sin_addr.s_addr == swap32(LOOPBACK) &&
	        swap16(from.sin_port) == sender_port, "recvfrom source");
	REQUIRE(fails(sys_recv(receiver, buffer, sizeof(buffer), MSG_DONTWAIT), EAGAIN), "empty queue");

	REQUIRE(sys_sendto(sender, "", 0, 0, &target, sizeof(target)) == 0, "empty datagram");
	REQUIRE(wait_datagram(receiver) == 0 && sys_recv(receiver, buffer, sizeof(buffer), 0) == 0, "empty datagram arrives");
	REQUIRE(sys_sendto(sender, "aaaa", 4, 0, &target, sizeof(target)) == 4 &&
	        sys_sendto(sender, "bb", 2, 0, &target, sizeof(target)) == 2, "two datagrams");
	REQUIRE(wait_datagram(receiver) == 0, "first of two");
	REQUIRE(ioctl(receiver, FIONREAD, &queued) == 0 && queued == 4, "FIONREAD is the next datagram");
	REQUIRE(sys_recv(receiver, buffer, sizeof(buffer), 0) == 4, "one datagram per receive");
	REQUIRE(wait_datagram(receiver) == 0 && sys_recv(receiver, buffer, sizeof(buffer), 0) == 2, "the second datagram");

	memset(big, 'z', 100);
	REQUIRE(sys_sendto(sender, big, 100, 0, &target, sizeof(target)) == 100, "100 bytes");
	REQUIRE(wait_datagram(receiver) == 0 && sys_recv(receiver, buffer, 10, 0) == 10, "truncated receive");
	REQUIRE(fails(sys_recv(receiver, buffer, sizeof(buffer), MSG_DONTWAIT), EAGAIN), "the rest is discarded");
	REQUIRE(sys_sendto(sender, big, 100, 0, &target, sizeof(target)) == 100, "100 bytes again");
	REQUIRE(wait_datagram(receiver) == 0 && sys_recv(receiver, buffer, 10, MSG_TRUNC) == 100, "MSG_TRUNC gives the length");
	REQUIRE(sys_sendto(sender, big, 100, 0, &target, sizeof(target)) == 100, "100 bytes for recvmsg");
	REQUIRE(wait_datagram(receiver) == 0, "for recvmsg");
	memset(&message, 0, sizeof(message));
	pieces[0] = (struct iovec){buffer, 10};
	message.msg_name = &from;
	message.msg_namelen = sizeof(from);
	message.msg_iov = pieces;
	message.msg_iovlen = 1;
	message.msg_control = control;
	message.msg_controllen = sizeof(control);
	REQUIRE(sys_recvmsg(receiver, &message, 0) == 10, "recvmsg truncated");
	REQUIRE((message.msg_flags & MSG_TRUNC) && message.msg_namelen == 16 && message.msg_controllen == 0, "recvmsg flags");

	REQUIRE(sys_sendto(sender, "peek", 4, 0, &target, sizeof(target)) == 4, "peek datagram");
	REQUIRE(wait_datagram(receiver) == 0, "peek arrives");
	REQUIRE(sys_recv(receiver, buffer, sizeof(buffer), MSG_PEEK) == 4 && !memcmp(buffer, "peek", 4), "MSG_PEEK");
	REQUIRE(sys_recv(receiver, buffer, sizeof(buffer), 0) == 4 && !memcmp(buffer, "peek", 4), "receive after peek");
	REQUIRE(fails(sys_recv(receiver, buffer, sizeof(buffer), MSG_DONTWAIT), EAGAIN), "peek left one");

	memset(&message, 0, sizeof(message));
	pieces[0] = (struct iovec){"ab", 2};
	pieces[1] = (struct iovec){"cde", 3};
	message.msg_name = &target;
	message.msg_namelen = sizeof(target);
	message.msg_iov = pieces;
	message.msg_iovlen = 2;
	REQUIRE(sys_sendmsg(sender, &message, 0) == 5, "sendmsg");
	REQUIRE(wait_datagram(receiver) == 0, "sendmsg arrives");
	memset(&message, 0, sizeof(message));
	pieces[0] = (struct iovec){first, 2};
	pieces[1] = (struct iovec){second, sizeof(second)};
	message.msg_name = &from;
	message.msg_namelen = sizeof(from);
	message.msg_iov = pieces;
	message.msg_iovlen = 2;
	REQUIRE(sys_recvmsg(receiver, &message, 0) == 5 && !memcmp(first, "ab", 2) && !memcmp(second, "cde", 3), "recvmsg scatter");
	REQUIRE(message.msg_flags == 0 && swap16(from.sin_port) == sender_port, "recvmsg source and flags");

	/* A connected socket takes datagrams only from its peer. */
	connected = sys_socket(AF_INET, SOCK_DGRAM, 0);
	REQUIRE(connected >= 0 && sys_connect(connected, &target, sizeof(target)) == 0, "connect");
	length = sizeof(peer);
	REQUIRE(sys_getpeername(connected, &peer, &length) == 0 && same_address(&peer, &target), "getpeername");
	peer = in4(LOOPBACK, (uint16_t)local_port(connected));
	REQUIRE(sys_connect(receiver, &peer, sizeof(peer)) == 0, "receiver connects back");
	stranger = sys_socket(AF_INET, SOCK_DGRAM, 0);
	REQUIRE(stranger >= 0 && sys_sendto(stranger, "stranger", 8, 0, &target, sizeof(target)) == 8, "stranger sends");
	REQUIRE(revents(receiver, POLLIN, 300) == 0, "stranger filtered");
	REQUIRE(sys_send(connected, "mine", 4, 0) == 4 && write(connected, "!", 1) == 1, "connected send and write");
	REQUIRE(wait_datagram(receiver) == 0 && read(receiver, buffer, sizeof(buffer)) == 4 && !memcmp(buffer, "mine", 4), "connected read");
	REQUIRE(wait_datagram(receiver) == 0 && read(receiver, buffer, sizeof(buffer)) == 1, "connected write");
	close(stranger);

	/* A datagram past the limit fails whole and nothing arrives. Linux
	 * carries 65507 bytes over IPv4 and GoOS 4096; 4096 fits both, 65508
	 * neither, and 65000 either arrives whole or fails. */
	for (i = 0; i < (int)sizeof(big); i++)
		big[i] = (char)pattern_byte((size_t)i);
	REQUIRE(fails(sys_send(connected, big, sizeof(big), 0), EMSGSIZE), "65508 bytes is EMSGSIZE");
	REQUIRE(revents(receiver, POLLIN, 200) == 0, "nothing of 65508 bytes arrives");
	REQUIRE(sys_send(connected, big, 4096, 0) == 4096, "4096 bytes");
	{
		static char back[65536];

		REQUIRE(wait_datagram(receiver) == 0, "4096 bytes arrive");
		n = sys_recv(receiver, back, sizeof(back), 0);
		if (trace)
			fprintf(stderr, "trace recv %ld errno=%d\n", n, n < 0 ? errno : 0);
		REQUIRE(n == 4096 && !memcmp(back, big, 4096), "4096 bytes whole");
		n = sys_send(connected, big, 65000, 0);
		if (n == 65000) {
			REQUIRE(wait_datagram(receiver) == 0 && sys_recv(receiver, back, sizeof(back), 0) == 65000 && !memcmp(back, big, 65000), "65000 bytes whole");
		} else {
			REQUIRE(fails(n, EMSGSIZE), "65000 bytes neither sent nor EMSGSIZE");
			REQUIRE(revents(receiver, POLLIN, 200) == 0, "nothing of 65000 bytes arrives");
		}
	}
	close(connected);
	close(sender);
	close(receiver);
	printf("udp ok\n");
	return 0;
}

/* --- dup --------------------------------------------------------------- */

static int dup_group(void)
{
	struct stat first, second;
	char buffer[16];
	int client, server, copy, shared, cloexec, status;
	pid_t child;

	REQUIRE(tcp_pair(&client, &server) == 0, "pair");
	copy = dup(client);
	REQUIRE(copy >= 0 && sys_fstat(client, &first) == 0 && sys_fstat(copy, &second) == 0 && first.st_ino == second.st_ino, "dup is one socket");
	close(client);
	REQUIRE(write(copy, "dup", 3) == 3, "write through the copy");
	REQUIRE(revents(server, POLLIN, 2000) & POLLIN, "copy's data arrives");
	REQUIRE(read(server, buffer, sizeof(buffer)) == 3, "read the copy's data");
	REQUIRE(nonblocking(copy, 1) == 0, "O_NONBLOCK on the copy");
	shared = dup(copy);
	REQUIRE(shared >= 0 && (fcntl(shared, F_GETFL) & O_NONBLOCK), "status flags shared");
	REQUIRE(fails(read(shared, buffer, sizeof(buffer)), EAGAIN), "shared nonblocking read");
	REQUIRE(nonblocking(shared, 0) == 0 && !(fcntl(copy, F_GETFL) & O_NONBLOCK), "status flags cleared for both");
	cloexec = fcntl(copy, F_DUPFD_CLOEXEC, 0);
	REQUIRE(cloexec >= 0 && fcntl(cloexec, F_GETFD) == FD_CLOEXEC && fcntl(copy, F_GETFD) == 0, "close-on-exec per descriptor");
	REQUIRE(dup3(copy, cloexec, O_CLOEXEC) == cloexec && fcntl(cloexec, F_GETFD) == FD_CLOEXEC, "dup3 O_CLOEXEC");
	REQUIRE(int_option(cloexec, SOL_SOCKET, SO_TYPE) == SOCK_STREAM, "the copy is a socket");

	child = spawn();
	if (child == 0) {
		_exit(write(copy, "child", 5) == 5 ? 0 : 1);
	}
	REQUIRE(child > 0, "fork");
	status = child_status(child);
	REQUIRE(WIFEXITED(status) && WEXITSTATUS(status) == 0, "child wrote");
	REQUIRE(revents(server, POLLIN, 2000) & POLLIN, "child's data arrives");
	REQUIRE(read(server, buffer, sizeof(buffer)) == 5 && !memcmp(buffer, "child", 5), "child's data");
	REQUIRE(revents(server, POLLIN | POLLRDHUP, 200) == 0, "the child's exit closes nothing");
	close(copy);
	close(shared);
	REQUIRE(revents(server, POLLIN | POLLRDHUP, 200) == 0, "one descriptor still open");
	close(cloexec);
	REQUIRE(revents(server, POLLIN, 2000) & POLLIN, "last close reaches the peer");
	REQUIRE(read(server, buffer, sizeof(buffer)) == 0, "end of file after the last close");
	close(server);
	printf("dup ok\n");
	return 0;
}

/* --- exec -------------------------------------------------------------- */

/* exec_check runs in the image execve started: kept must still be an open
 * stream socket and dropped must be closed. */
static int exec_check(int kept, int dropped)
{
	if (fcntl(kept, F_GETFD) != 0)
		return 2;
	if (int_option(kept, SOL_SOCKET, SO_TYPE) != SOCK_STREAM)
		return 3;
	if (!fails(fcntl(dropped, F_GETFD), EBADF))
		return 4;
	return write(kept, "exec", 4) == 4 ? 0 : 5;
}

static int exec_group(void)
{
	char kept_text[16], dropped_text[16], buffer[16];
	int client, server, dropped, status;
	pid_t child;

	REQUIRE(tcp_pair(&client, &server) == 0, "pair");
	dropped = sys_socket(AF_INET, SOCK_STREAM | SOCK_CLOEXEC, 0);
	REQUIRE(dropped >= 0, "SOCK_CLOEXEC socket");
	snprintf(kept_text, sizeof(kept_text), "%d", client);
	snprintf(dropped_text, sizeof(dropped_text), "%d", dropped);
	child = spawn();
	if (child == 0) {
		char *arguments[] = {(char *)self, "exec-check", kept_text, dropped_text, NULL};
		char *environment[] = {NULL};

		execve(self, arguments, environment);
		_exit(9);
	}
	REQUIRE(child > 0, "fork");
	status = child_status(child);
	errno = WIFEXITED(status) ? WEXITSTATUS(status) : 100 + WTERMSIG(status);
	REQUIRE(WIFEXITED(status) && WEXITSTATUS(status) == 0, "execve'd image check (errno is its step)");
	REQUIRE(revents(server, POLLIN, 2000) & POLLIN, "data from the new image");
	REQUIRE(read(server, buffer, sizeof(buffer)) == 4 && !memcmp(buffer, "exec", 4), "the new image wrote");
	close(dropped);
	close(client);
	REQUIRE(revents(server, POLLIN, 2000) & POLLIN && read(server, buffer, sizeof(buffer)) == 0, "last close after exec");
	close(server);
	printf("exec ok\n");
	return 0;
}

/* --- eintr ------------------------------------------------------------- */

/* poke forks a helper that sends SIGUSR1 to the caller after delay, and,
 * with a target, connects to it or writes to it after another 200 ms. */
static pid_t poke(int delay, uint16_t connect_port, int write_fd)
{
	pid_t parent = getpid(), child = spawn();

	if (child != 0)
		return child;
	nap(delay);
	if (kill(parent, SIGUSR1) != 0 || trace)
		fprintf(stderr, "trace poke kill errno=%d\n", errno);
	if (connect_port != 0 || write_fd >= 0)
		nap(200);
	if (connect_port != 0) {
		int fd = sys_socket(AF_INET, SOCK_STREAM, 0);

		connect4(fd, LOOPBACK, connect_port);
		nap(500);
	}
	if (write_fd >= 0)
		write(write_fd, "late", 4);
	_exit(0);
}

static int eintr_group(void)
{
	struct timeval timeout = {2, 0};
	char buffer[16];
	int fd, client, server, accepted;
	uint16_t port;
	pid_t helper;

	REQUIRE(handle(SIGUSR1, count_signal, 0) == 0, "handler");
	fd = listener(&port, 0);
	REQUIRE(fd >= 0, "listener");
	signals_seen = 0;
	helper = poke(100, 0, -1);
	REQUIRE(fails(sys_accept4(fd, NULL, NULL, 0), EINTR) && signals_seen == 1, "accept interrupted");
	child_status(helper);
	REQUIRE(tcp_pair(&client, &server) == 0, "pair");
	signals_seen = 0;
	helper = poke(100, 0, -1);
	REQUIRE(fails(read(server, buffer, sizeof(buffer)), EINTR) && signals_seen == 1, "read interrupted");
	child_status(helper);

	REQUIRE(handle(SIGUSR1, count_signal, SA_RESTART) == 0, "SA_RESTART handler");
	signals_seen = 0;
	helper = poke(100, port, -1);
	accepted = sys_accept4(fd, NULL, NULL, 0);
	REQUIRE(accepted >= 0 && signals_seen == 1, "accept restarted");
	close(accepted);
	child_status(helper);
	signals_seen = 0;
	helper = poke(100, 0, client);
	REQUIRE(read(server, buffer, sizeof(buffer)) == 4 && !memcmp(buffer, "late", 4) && signals_seen == 1, "read restarted");
	child_status(helper);

	/* A socket with a receive timeout is not restarted. */
	REQUIRE(sys_setsockopt(server, SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof(timeout)) == 0, "SO_RCVTIMEO");
	signals_seen = 0;
	helper = poke(100, 0, -1);
	REQUIRE(fails(read(server, buffer, sizeof(buffer)), EINTR) && signals_seen == 1, "read with a timeout interrupted");
	child_status(helper);
	signal(SIGUSR1, SIG_DFL);
	close_pair(client, server);
	close(fd);
	printf("eintr ok\n");
	return 0;
}

/* --- options ----------------------------------------------------------- */

static int options_group(void)
{
	struct timeval timeout = {0, 200000}, back;
	socklen_t length;
	char buffer[16];
	int fd, udp, client, server, value, size, ttl, segment;
	long start, elapsed;
	short twobytes = 1;

	fd = sys_socket(AF_INET, SOCK_STREAM, 0);
	udp = sys_socket(AF_INET, SOCK_DGRAM, 0);
	REQUIRE(fd >= 0 && udp >= 0, "sockets");
	REQUIRE(int_option(fd, SOL_SOCKET, SO_TYPE) == SOCK_STREAM && int_option(udp, SOL_SOCKET, SO_TYPE) == SOCK_DGRAM, "SO_TYPE");
	REQUIRE(int_option(fd, SOL_SOCKET, SO_DOMAIN) == AF_INET && int_option(udp, SOL_SOCKET, SO_DOMAIN) == AF_INET, "SO_DOMAIN");
	REQUIRE(int_option(fd, SOL_SOCKET, SO_ERROR) == 0, "SO_ERROR");
	REQUIRE(int_option(fd, SOL_SOCKET, SO_REUSEADDR) == 0, "SO_REUSEADDR off");
	REQUIRE(set_int_option(fd, SOL_SOCKET, SO_REUSEADDR, 1) == 0 && int_option(fd, SOL_SOCKET, SO_REUSEADDR) == 1, "SO_REUSEADDR on");
	REQUIRE(fails(sys_setsockopt(fd, SOL_SOCKET, SO_REUSEADDR, &twobytes, sizeof(twobytes)), EINVAL), "short option value");

	/* Buffer sizes are effective capacities: positive, never below a
	 * small request, clamped for a large one. */
	REQUIRE(int_option(fd, SOL_SOCKET, SO_RCVBUF) > 0 && int_option(fd, SOL_SOCKET, SO_SNDBUF) > 0, "buffer sizes");
	REQUIRE(set_int_option(fd, SOL_SOCKET, SO_RCVBUF, 4096) == 0 && int_option(fd, SOL_SOCKET, SO_RCVBUF) >= 4096, "SO_RCVBUF 4096");
	REQUIRE(set_int_option(fd, SOL_SOCKET, SO_SNDBUF, 4096) == 0 && int_option(fd, SOL_SOCKET, SO_SNDBUF) >= 4096, "SO_SNDBUF 4096");
	REQUIRE(set_int_option(fd, SOL_SOCKET, SO_RCVBUF, 1 << 30) == 0 && (size = int_option(fd, SOL_SOCKET, SO_RCVBUF)) > 0, "SO_RCVBUF large");
	REQUIRE(set_int_option(udp, SOL_SOCKET, SO_RCVBUF, 4096) == 0 && int_option(udp, SOL_SOCKET, SO_RCVBUF) >= 4096, "UDP SO_RCVBUF");

	REQUIRE(sys_setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &timeout, sizeof(timeout)) == 0, "SO_SNDTIMEO");
	length = sizeof(back);
	REQUIRE(sys_getsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &back, &length) == 0 && length == sizeof(back) &&
	        back.tv_sec == 0 && back.tv_usec > 150000 && back.tv_usec <= 250000, "SO_SNDTIMEO back");

	value = 9999;
	length = sizeof(value);
	REQUIRE(fails(sys_getsockopt(fd, SOL_SOCKET, 9999, &value, &length), ENOPROTOOPT), "unknown SOL_SOCKET option");
	REQUIRE(fails(set_int_option(fd, SOL_SOCKET, 9999, 1), ENOPROTOOPT), "set unknown SOL_SOCKET option");
	length = sizeof(value);
	REQUIRE(fails(sys_getsockopt(fd, IPPROTO_TCP, 9999, &value, &length), ENOPROTOOPT), "unknown TCP option");
	length = sizeof(value);
	REQUIRE(fails(sys_getsockopt(fd, 999, 1, &value, &length), EOPNOTSUPP), "unknown level");
	close(fd);
	close(udp);

	REQUIRE(tcp_pair(&client, &server) == 0, "pair");
	REQUIRE(set_int_option(client, IPPROTO_TCP, TCP_NODELAY, 1) == 0 && int_option(client, IPPROTO_TCP, TCP_NODELAY) == 1, "TCP_NODELAY");
	segment = int_option(client, IPPROTO_TCP, TCP_MAXSEG);
	REQUIRE(segment > 0 && segment <= 65535, "TCP_MAXSEG");
	ttl = int_option(client, IPPROTO_IP, IP_TTL);
	REQUIRE(ttl >= 1 && ttl <= 255, "IP_TTL");
	REQUIRE(sys_setsockopt(server, SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof(timeout)) == 0, "SO_RCVTIMEO");
	length = sizeof(back);
	REQUIRE(sys_getsockopt(server, SOL_SOCKET, SO_RCVTIMEO, &back, &length) == 0 && back.tv_sec == 0 &&
	        back.tv_usec > 150000 && back.tv_usec <= 250000, "SO_RCVTIMEO back");
	start = now_ms();
	REQUIRE(fails(read(server, buffer, sizeof(buffer)), EAGAIN), "receive timeout");
	elapsed = now_ms() - start;
	REQUIRE(elapsed >= 150 && elapsed < 3000, "receive timeout duration");
	close_pair(client, server);
	(void)size;
	printf("options ok\n");
	return 0;
}

/* --- file -------------------------------------------------------------- */

static int file_group(void)
{
	struct stat status;
	char buffer[4];
	int fd, listening;
	uint16_t port;

	fd = sys_socket(AF_INET, SOCK_STREAM | SOCK_NONBLOCK, 0);
	REQUIRE(fd >= 0 && sys_fstat(fd, &status) == 0 && S_ISSOCK(status.st_mode), "fstat S_IFSOCK");
	REQUIRE(fails(lseek(fd, 0, SEEK_SET), ESPIPE), "lseek");
	REQUIRE(fails(fsync(fd), EINVAL), "fsync");
	REQUIRE(!isatty(fd) && errno == ENOTTY, "not a terminal");
	REQUIRE(fails(read(fd, buffer, sizeof(buffer)), ENOTCONN), "read unconnected");
	close(fd);
	listening = listener(&port, SOCK_NONBLOCK);
	REQUIRE(listening >= 0 && fails(read(listening, buffer, sizeof(buffer)), ENOTCONN), "read a listener");
	close(listening);
	printf("file ok\n");
	return 0;
}

/* --- loopback groups --------------------------------------------------- */

struct probe_group {
	const char *name;
	int (*run)(void);
};

static const struct probe_group groups[] = {
	{"create", create_group},
	{"sockaddr", sockaddr_group},
	{"bind", bind_group},
	{"listen", listen_group},
	{"connect", connect_group},
	{"stream", stream_group},
	{"shutdown", shutdown_group},
	{"sigpipe", sigpipe_group},
	{"poll", poll_group},
	{"select", select_group},
	{"udp", udp_group},
	{"dup", dup_group},
	{"exec", exec_group},
	{"eintr", eintr_group},
	{"options", options_group},
	{"file", file_group},
};

static int run_group(const struct probe_group *entry)
{
	int result;

	group = entry->name;
	signal(SIGPIPE, SIG_IGN);
	errno = 0;
	result = entry->run();
	fflush(stdout);
	return result;
}

/* --- SHA-256 ----------------------------------------------------------- */

struct sha256 {
	uint32_t state[8];
	unsigned char block[64];
	uint64_t length;
	size_t used;
};

static const uint32_t sha_k[64] = {
	0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
	0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
	0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
	0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
	0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
	0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
	0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
	0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
};

#define ROTR(x, n) (((x) >> (n)) | ((x) << (32 - (n))))

static void sha256_block(struct sha256 *s)
{
	uint32_t w[64], a, b, c, d, e, f, g, h, t1, t2;
	int i;

	for (i = 0; i < 16; i++)
		w[i] = (uint32_t)s->block[i * 4] << 24 | (uint32_t)s->block[i * 4 + 1] << 16 |
		       (uint32_t)s->block[i * 4 + 2] << 8 | s->block[i * 4 + 3];
	for (i = 16; i < 64; i++)
		w[i] = w[i - 16] + (ROTR(w[i - 15], 7) ^ ROTR(w[i - 15], 18) ^ (w[i - 15] >> 3)) + w[i - 7] +
		       (ROTR(w[i - 2], 17) ^ ROTR(w[i - 2], 19) ^ (w[i - 2] >> 10));
	a = s->state[0], b = s->state[1], c = s->state[2], d = s->state[3];
	e = s->state[4], f = s->state[5], g = s->state[6], h = s->state[7];
	for (i = 0; i < 64; i++) {
		t1 = h + (ROTR(e, 6) ^ ROTR(e, 11) ^ ROTR(e, 25)) + ((e & f) ^ (~e & g)) + sha_k[i] + w[i];
		t2 = (ROTR(a, 2) ^ ROTR(a, 13) ^ ROTR(a, 22)) + ((a & b) ^ (a & c) ^ (b & c));
		h = g, g = f, f = e, e = d + t1, d = c, c = b, b = a, a = t1 + t2;
	}
	s->state[0] += a, s->state[1] += b, s->state[2] += c, s->state[3] += d;
	s->state[4] += e, s->state[5] += f, s->state[6] += g, s->state[7] += h;
}

static void sha256_start(struct sha256 *s)
{
	static const uint32_t initial[8] = {0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a,
	                                    0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19};

	memcpy(s->state, initial, sizeof(initial));
	s->length = 0;
	s->used = 0;
}

static void sha256_add(struct sha256 *s, const unsigned char *data, size_t size)
{
	while (size--) {
		s->block[s->used++] = *data++;
		s->length++;
		if (s->used == 64) {
			sha256_block(s);
			s->used = 0;
		}
	}
}

static void sha256_hex(struct sha256 *s, char out[65])
{
	uint64_t bits = s->length * 8;
	unsigned char pad = 0x80, zero = 0, size[8];
	int i;

	sha256_add(s, &pad, 1);
	while (s->used != 56)
		sha256_add(s, &zero, 1);
	for (i = 0; i < 8; i++)
		size[i] = (unsigned char)(bits >> (56 - i * 8));
	sha256_add(s, size, 8);
	for (i = 0; i < 8; i++)
		snprintf(out + i * 8, 9, "%08x", s->state[i]);
}

/* --- resolver ---------------------------------------------------------- */

static int parse_ipv4(const char *text, uint32_t *address)
{
	unsigned long part;
	uint32_t value = 0;
	int i;
	char *end;

	for (i = 0; i < 4; i++) {
		if (*text < '0' || *text > '9')
			return -1;
		part = strtoul(text, &end, 10);
		if (part > 255 || (i < 3 && *end != '.') || (i == 3 && *end != 0 && *end != ' ' && *end != '\t' && *end != '\n'))
			return -1;
		value = value << 8 | (uint32_t)part;
		text = end + 1;
	}
	*address = value;
	return 0;
}

static void format_ipv4(uint32_t address, char out[16])
{
	snprintf(out, 16, "%u.%u.%u.%u", address >> 24, address >> 16 & 255, address >> 8 & 255, address & 255);
}

/* read_small reads a whole small file into buffer and terminates it. */
static int read_small(const char *path, char *buffer, size_t size)
{
	int fd = open(path, O_RDONLY | O_CLOEXEC);
	long n, total = 0;

	if (fd < 0)
		return -1;
	while (total < (long)size - 1 && (n = read(fd, buffer + total, size - 1 - total)) > 0)
		total += n;
	close(fd);
	buffer[total] = 0;
	return 0;
}

static int from_hosts(const char *name, uint32_t *address)
{
	static char text[8192];
	char *line, *next, *field, *save;

	if (read_small("/etc/hosts", text, sizeof(text)) != 0)
		return -1;
	for (line = text; line && *line; line = next) {
		uint32_t candidate;

		next = strchr(line, '\n');
		if (next)
			*next++ = 0;
		if (strchr(line, '#'))
			*strchr(line, '#') = 0;
		field = strtok_r(line, " \t", &save);
		if (!field || parse_ipv4(field, &candidate) != 0)
			continue;
		while ((field = strtok_r(NULL, " \t\r", &save)) != NULL)
			if (!strcasecmp(field, name)) {
				*address = candidate;
				return 0;
			}
	}
	return -1;
}

static uint32_t nameserver(void)
{
	static char text[4096];
	char *line;
	uint32_t address;

	if (read_small("/etc/resolv.conf", text, sizeof(text)) == 0)
		for (line = text; line; line = strchr(line, '\n') ? strchr(line, '\n') + 1 : NULL)
			if (!strncmp(line, "nameserver", 10) && (line[10] == ' ' || line[10] == '\t')) {
				line += 11;
				while (*line == ' ' || *line == '\t')
					line++;
				if (parse_ipv4(line, &address) == 0)
					return address;
			}
	return LOOPBACK;
}

static int build_query(const char *name, uint16_t id, unsigned char *query)
{
	size_t length = strlen(name), at = 12, start = 0, i;

	if (length == 0 || length > 253)
		return -1;
	memset(query, 0, 12);
	query[0] = (unsigned char)(id >> 8);
	query[1] = (unsigned char)id;
	query[2] = 0x01; /* RD */
	query[5] = 1;    /* one question */
	for (i = 0; i <= length; i++) {
		if (i == length || name[i] == '.') {
			size_t label = i - start;

			if (label == 0 && i == length && i > 0)
				break; /* trailing dot */
			if (label == 0 || label > 63)
				return -1;
			query[at++] = (unsigned char)label;
			memcpy(query + at, name + start, label);
			at += label;
			start = i + 1;
		}
	}
	query[at++] = 0;
	query[at++] = 0, query[at++] = 1; /* A */
	query[at++] = 0, query[at++] = 1; /* IN */
	return (int)at;
}

static int skip_name(const unsigned char *message, int size, int at)
{
	while (at < size) {
		if (message[at] == 0)
			return at + 1;
		if ((message[at] & 0xc0) == 0xc0)
			return at + 2 <= size ? at + 2 : -1;
		at += message[at] + 1;
	}
	return -1;
}

/* parse_answer returns 0 with an address, 1 for a truncated answer, 2 for
 * NXDOMAIN, 3 for no address, and -1 for a reply that is not ours. */
static int parse_answer(const unsigned char *message, int size, uint16_t id, uint32_t *address)
{
	int answers, at, i;

	if (size < 12 || message[0] != (id >> 8) || message[1] != (id & 255) || !(message[2] & 0x80))
		return -1;
	if (message[2] & 0x02)
		return 1;
	if ((message[3] & 15) == 3)
		return 2;
	if ((message[3] & 15) != 0)
		return 3;
	answers = message[6] << 8 | message[7];
	at = skip_name(message, size, 12);
	if (at < 0 || at + 4 > size)
		return -1;
	at += 4;
	for (i = 0; i < answers; i++) {
		int type, rdlength;

		at = skip_name(message, size, at);
		if (at < 0 || at + 10 > size)
			return 3;
		type = message[at] << 8 | message[at + 1];
		rdlength = message[at + 8] << 8 | message[at + 9];
		at += 10;
		if (at + rdlength > size)
			return 3;
		if (type == 1 && rdlength == 4) {
			*address = (uint32_t)message[at] << 24 | (uint32_t)message[at + 1] << 16 |
			           (uint32_t)message[at + 2] << 8 | message[at + 3];
			return 0;
		}
		at += rdlength;
	}
	return 3;
}

static int query_udp(uint32_t server, const unsigned char *query, int length, uint16_t id, uint32_t *address)
{
	unsigned char reply[4096];
	int fd, attempt, result = 3;
	long n;

	fd = sys_socket(AF_INET, SOCK_DGRAM | SOCK_CLOEXEC, 0);
	if (fd < 0 || connect4(fd, server, 53) != 0) {
		if (fd >= 0)
			close(fd);
		return -2;
	}
	for (attempt = 0; attempt < 2; attempt++) {
		long deadline = now_ms() + 3000;

		if (sys_send(fd, query, (size_t)length, 0) != length)
			break;
		while (now_ms() < deadline) {
			if (!(revents(fd, POLLIN, (int)(deadline - now_ms())) & POLLIN))
				break;
			n = sys_recv(fd, reply, sizeof(reply), 0);
			if (n < 0)
				break;
			result = parse_answer(reply, (int)n, id, address);
			if (result >= 0) {
				close(fd);
				return result;
			}
		}
	}
	close(fd);
	return -2;
}

static int query_tcp(uint32_t server, const unsigned char *query, int length, uint16_t id, uint32_t *address)
{
	static unsigned char reply[65536];
	unsigned char prefix[2] = {(unsigned char)(length >> 8), (unsigned char)length};
	struct timeval timeout = {5, 0};
	int fd, size, result;

	fd = sys_socket(AF_INET, SOCK_STREAM | SOCK_CLOEXEC, 0);
	if (fd < 0)
		return -2;
	sys_setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof(timeout));
	if (connect4(fd, server, 53) != 0 || write_full(fd, prefix, 2) != 0 || write_full(fd, query, (size_t)length) != 0 ||
	    read_full(fd, prefix, 2) != 0) {
		close(fd);
		return -2;
	}
	size = prefix[0] << 8 | prefix[1];
	result = read_full(fd, reply, (size_t)size) == 0 ? parse_answer(reply, size, id, address) : -2;
	close(fd);
	return result == 1 ? 3 : result;
}

/* resolve returns 0 with an address, or 2 for a name that does not exist
 * and 3 for any other failure. via says how the answer came. */
static int resolve(const char *name, uint32_t *address, const char **via)
{
	unsigned char query[300];
	uint16_t id = (uint16_t)(getpid() * 7919 + now_ms());
	uint32_t server;
	int length, result;

	*via = "numeric";
	if (parse_ipv4(name, address) == 0)
		return 0;
	*via = "hosts";
	if (from_hosts(name, address) == 0)
		return 0;
	length = build_query(name, id, query);
	if (length < 0)
		return 3;
	server = nameserver();
	*via = "udp";
	result = query_udp(server, query, length, id, address);
	if (result == 1) {
		*via = "tcp";
		result = query_tcp(server, query, length, id, address);
	}
	return result == 0 ? 0 : result == 2 ? 2 : 3;
}

static int resolve_mode(const char *name)
{
	char text[16];
	const char *via;
	uint32_t address;
	int result = resolve(name, &address, &via);

	if (result != 0) {
		printf("resolve %s failed: %s\n", name, result == 2 ? "name not found" : "temporary failure");
		return 2;
	}
	format_ipv4(address, text);
	printf("resolve %s %s via %s\n", name, text, via);
	return 0;
}

/* --- clients ----------------------------------------------------------- */

static int parse_port(const char *text)
{
	char *end;
	long value = strtol(text, &end, 10);

	return *end == 0 && value > 0 && value < 65536 ? (int)value : -1;
}

/* open_client resolves host, connects, and returns the socket or -1 with a
 * message printed. */
static int open_client(const char *mode, const char *host, int port, int wait_nonblocking)
{
	const char *via;
	uint32_t address;
	int fd, events;

	if (port < 0 || resolve(host, &address, &via) != 0) {
		printf("%s failed: cannot resolve %s\n", mode, host);
		return -1;
	}
	fd = sys_socket(AF_INET, SOCK_STREAM | (wait_nonblocking ? SOCK_NONBLOCK : 0), 0);
	if (fd < 0) {
		printf("%s failed: socket errno=%s\n", mode, errno_name(errno));
		return -1;
	}
	if (!wait_nonblocking) {
		if (connect4(fd, address, (uint16_t)port) != 0) {
			printf("%s failed: connect errno=%s\n", mode, errno_name(errno));
			close(fd);
			return -1;
		}
		return fd;
	}
	if (connect4(fd, address, (uint16_t)port) != 0 && errno != EINPROGRESS) {
		printf("%s failed: connect errno=%s\n", mode, errno_name(errno));
		close(fd);
		return -1;
	}
	events = revents(fd, POLLOUT, 10000);
	if (events <= 0 || int_option(fd, SOL_SOCKET, SO_ERROR) != 0) {
		printf("%s failed: nonblocking connect events=%d\n", mode, events);
		close(fd);
		return -1;
	}
	nonblocking(fd, 0);
	return fd;
}

static int fetch_mode(int argc, char **argv)
{
	static char header[8192];
	unsigned char data[16384];
	struct timeval timeout = {30, 0};
	struct sha256 hash;
	char request[512], digest[65];
	int wait_nonblocking = argc > 2 && !strcmp(argv[2], "-n");
	int base = wait_nonblocking ? 3 : 2, fd, status = 0;
	size_t used = 0;
	long body = 0, n;
	char *end;

	if (argc != base + 3) {
		printf("usage: netprobe fetch [-n] HOST PORT PATH\n");
		return 2;
	}
	fd = open_client("fetch", argv[base], parse_port(argv[base + 1]), wait_nonblocking);
	if (fd < 0)
		return 1;
	sys_setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof(timeout));
	snprintf(request, sizeof(request), "GET %s HTTP/1.0\r\nHost: %s\r\nUser-Agent: netprobe\r\n\r\n", argv[base + 2], argv[base]);
	if (write_full(fd, request, strlen(request)) != 0) {
		printf("fetch failed: write errno=%s\n", errno_name(errno));
		return 1;
	}
	sha256_start(&hash);
	for (;;) {
		n = read(fd, data, sizeof(data));
		if (n < 0 && errno == EINTR)
			continue;
		if (n < 0) {
			printf("fetch failed: read errno=%s after %ld bytes\n", errno_name(errno), body);
			return 1;
		}
		if (n == 0)
			break;
		if (status == 0) {
			size_t take = (size_t)n < sizeof(header) - 1 - used ? (size_t)n : sizeof(header) - 1 - used;

			memcpy(header + used, data, take);
			used += take;
			header[used] = 0;
			end = strstr(header, "\r\n\r\n");
			if (!end) {
				if (used == sizeof(header) - 1) {
					printf("fetch failed: header too long\n");
					return 1;
				}
				continue;
			}
			if (strncmp(header, "HTTP/1.", 7) != 0 || strlen(header) < 12) {
				printf("fetch failed: not an HTTP response\n");
				return 1;
			}
			status = atoi(header + 9);
			if (status <= 0)
				status = -1;
			{
				size_t start = (size_t)(end + 4 - header);

				sha256_add(&hash, (unsigned char *)header + start, used - start);
				body += (long)(used - start);
				/* Bytes of this read past what fit in header. */
				sha256_add(&hash, data + take, (size_t)n - take);
				body += n - (long)take;
			}
			continue;
		}
		sha256_add(&hash, data, (size_t)n);
		body += n;
	}
	close(fd);
	sha256_hex(&hash, digest);
	printf("fetch status=%d bytes=%ld sha256=%s\n", status, body, digest);
	return status == 200 ? 0 : 1;
}

static int connect_mode(int argc, char **argv)
{
	const char *via;
	uint32_t address;
	int port, wait, fd, events, error, cleared, blocking;
	long start;

	if (argc != 5 || (port = parse_port(argv[3])) < 0 || (wait = atoi(argv[4])) <= 0 || resolve(argv[2], &address, &via) != 0) {
		printf("usage: netprobe connect HOST PORT MS\n");
		return 2;
	}
	fd = sys_socket(AF_INET, SOCK_STREAM | SOCK_NONBLOCK, 0);
	if (fd < 0 || connect4(fd, address, (uint16_t)port) == 0 || errno != EINPROGRESS) {
		printf("connect failed: nonblocking connect errno=%s\n", errno_name(errno));
		return 1;
	}
	start = now_ms();
	events = revents(fd, POLLOUT, wait);
	if (events == 0) {
		/* The attempt is abandoned while it is in progress. */
		close(fd);
		printf("connect result=timeout waited=%s\n", now_ms() - start >= wait - wait / 10 ? "ok" : "short");
		return 0;
	}
	error = int_option(fd, SOL_SOCKET, SO_ERROR);
	cleared = int_option(fd, SOL_SOCKET, SO_ERROR) == 0;
	close(fd);
	blocking = sys_socket(AF_INET, SOCK_STREAM, 0);
	errno = 0;
	connect4(blocking, address, (uint16_t)port);
	printf("connect result=%s pollerr=%d cleared=%d blocking=%s\n", errno_name(error), !!(events & POLLERR), cleared, errno_name(errno));
	close(blocking);
	return 0;
}

static int reset_mode(int argc, char **argv)
{
	char buffer[64];
	int fd, read_errno, write_errno;
	long n;

	if (argc != 4) {
		printf("usage: netprobe reset HOST PORT\n");
		return 2;
	}
	fd = open_client("reset", argv[2], parse_port(argv[3]), 0);
	if (fd < 0)
		return 1;
	if (write(fd, "reset\n", 6) != 6) {
		printf("reset failed: write errno=%s\n", errno_name(errno));
		return 1;
	}
	n = read(fd, buffer, sizeof(buffer));
	read_errno = n < 0 ? errno : 0;
	nap(100);
	errno = 0;
	sys_send(fd, "x", 1, MSG_NOSIGNAL);
	write_errno = errno;
	printf("reset read=%s write=%s\n", n < 0 ? errno_name(read_errno) : n == 0 ? "eof" : "data", errno_name(write_errno));
	close(fd);
	return 0;
}

static int silent_mode(int argc, char **argv)
{
	struct timeval timeout;
	char buffer[16];
	int fd, wait;
	long start, elapsed, n;

	if (argc != 5 || (wait = atoi(argv[4])) <= 0) {
		printf("usage: netprobe silent HOST PORT MS\n");
		return 2;
	}
	fd = open_client("silent", argv[2], parse_port(argv[3]), 0);
	if (fd < 0)
		return 1;
	timeout.tv_sec = wait / 1000;
	timeout.tv_usec = (wait % 1000) * 1000;
	sys_setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof(timeout));
	start = now_ms();
	n = read(fd, buffer, sizeof(buffer));
	elapsed = now_ms() - start;
	printf("silent read=%s elapsed=%s\n", n < 0 ? errno_name(errno) : "data",
	       elapsed >= wait - wait / 10 && elapsed < wait * 5 + 1000 ? "ok" : "wrong");
	close(fd);
	return 0;
}

static int echo_once(int fd, const struct sockaddr_in *peer, const char *text, char *back, size_t size, struct sockaddr_in *from)
{
	socklen_t length = sizeof(*from);
	int attempt;
	long n;

	for (attempt = 0; attempt < 3; attempt++) {
		if (peer)
			n = sys_sendto(fd, text, strlen(text), 0, peer, sizeof(*peer));
		else
			n = sys_send(fd, text, strlen(text), 0);
		if (n != (long)strlen(text))
			return -1;
		if (revents(fd, POLLIN, 3000) & POLLIN) {
			n = sys_recvfrom(fd, back, size, 0, from, &length);
			return (int)n;
		}
	}
	return -1;
}

static int udp_echo_mode(int argc, char **argv)
{
	struct sockaddr_in peer, from;
	struct msghdr message;
	struct iovec piece;
	char back[256], big[100];
	const char *via;
	uint32_t address;
	socklen_t length;
	int fd, port;

	group = "udp-echo";
	if (argc != 4 || (port = parse_port(argv[3])) < 0 || resolve(argv[2], &address, &via) != 0) {
		printf("usage: netprobe udp-echo HOST PORT\n");
		return 2;
	}
	peer = in4(address, (uint16_t)port);

	fd = sys_socket(AF_INET, SOCK_DGRAM, 0);
	REQUIRE(fd >= 0, "socket");
	REQUIRE(echo_once(fd, &peer, "unconnected", back, sizeof(back), &from) == 11 && !memcmp(back, "unconnected", 11), "unconnected echo");
	REQUIRE(same_address(&from, &peer), "echo source");
	close(fd);

	fd = sys_socket(AF_INET, SOCK_DGRAM | SOCK_NONBLOCK, 0);
	REQUIRE(fd >= 0 && bind4(fd, INADDR_ANY, 0) == 0, "nonblocking socket");
	REQUIRE(fails(sys_recv(fd, back, sizeof(back), 0), EAGAIN), "nothing yet");
	REQUIRE(echo_once(fd, &peer, "nonblocking", back, sizeof(back), &from) == 11, "nonblocking echo");
	close(fd);

	fd = sys_socket(AF_INET, SOCK_DGRAM, 0);
	REQUIRE(fd >= 0 && sys_connect(fd, &peer, sizeof(peer)) == 0, "connect");
	length = sizeof(from);
	REQUIRE(sys_getpeername(fd, &from, &length) == 0 && same_address(&from, &peer), "getpeername");
	REQUIRE(echo_once(fd, NULL, "connected", back, sizeof(back), &from) == 9 && !memcmp(back, "connected", 9), "connected echo");
	memset(big, 'u', sizeof(big));
	big[sizeof(big) - 1] = 0;
	REQUIRE(sys_send(fd, big, 99, 0) == 99 && (revents(fd, POLLIN, 3000) & POLLIN), "long echo");
	memset(&message, 0, sizeof(message));
	piece = (struct iovec){back, 10};
	message.msg_iov = &piece;
	message.msg_iovlen = 1;
	REQUIRE(sys_recvmsg(fd, &message, 0) == 10 && (message.msg_flags & MSG_TRUNC), "truncated echo");
	REQUIRE(fails(sys_recv(fd, back, sizeof(back), MSG_DONTWAIT), EAGAIN), "the rest is discarded");
	REQUIRE(sys_send(fd, big, 99, 0) == 99 && (revents(fd, POLLIN, 3000) & POLLIN), "long echo again");
	REQUIRE(sys_recv(fd, back, 10, MSG_PEEK | MSG_TRUNC) == 99, "MSG_PEEK|MSG_TRUNC length");
	REQUIRE(read(fd, back, sizeof(back)) == 99, "connected read");
	close(fd);
	printf("udp-echo ok\n");
	return 0;
}

static int inherit_mode(int argc, char **argv)
{
	char kept_text[16], dropped_text[16], buffer[64];
	int fd, dropped, status;
	pid_t child;

	group = "inherit";
	if (argc != 4) {
		printf("usage: netprobe inherit HOST PORT\n");
		return 2;
	}
	fd = open_client("inherit", argv[2], parse_port(argv[3]), 0);
	if (fd < 0)
		return 1;
	child = spawn();
	if (child == 0)
		_exit(write(fd, "fork", 4) == 4 ? 0 : 1);
	REQUIRE(child > 0, "fork");
	status = child_status(child);
	REQUIRE(WIFEXITED(status) && WEXITSTATUS(status) == 0, "child wrote");
	REQUIRE(read_full(fd, buffer, 4) == 0 && !memcmp(buffer, "fork", 4), "echo of the child's bytes");
	dropped = sys_socket(AF_INET, SOCK_DGRAM | SOCK_CLOEXEC, 0);
	REQUIRE(dropped >= 0, "SOCK_CLOEXEC socket");
	snprintf(kept_text, sizeof(kept_text), "%d", fd);
	snprintf(dropped_text, sizeof(dropped_text), "%d", dropped);
	child = spawn();
	if (child == 0) {
		char *arguments[] = {(char *)self, "exec-check", kept_text, dropped_text, NULL};
		char *environment[] = {NULL};

		execve(self, arguments, environment);
		_exit(9);
	}
	REQUIRE(child > 0, "fork for exec");
	status = child_status(child);
	errno = WIFEXITED(status) ? WEXITSTATUS(status) : 100 + WTERMSIG(status);
	REQUIRE(WIFEXITED(status) && WEXITSTATUS(status) == 0, "execve'd image check (errno is its step)");
	REQUIRE(read_full(fd, buffer, 4) == 0 && !memcmp(buffer, "exec", 4), "echo of the new image's bytes");
	REQUIRE(write(fd, "parent", 6) == 6 && read_full(fd, buffer, 6) == 0 && !memcmp(buffer, "parent", 6), "parent still connected");
	close(dropped);
	close(fd);
	printf("inherit ok\n");
	return 0;
}

/* --- server ------------------------------------------------------------ */

static int serve_one(int fd, const char *path, int number)
{
	static char chunk[16384];
	struct timeval timeout = {10, 0};
	struct stat status;
	char request[4096], header[128];
	size_t used = 0;
	long n, sent = 0;
	int file;

	sys_setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof(timeout));
	sys_setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &timeout, sizeof(timeout));
	while (used < sizeof(request) - 1) {
		n = read(fd, request + used, sizeof(request) - 1 - used);
		if (n <= 0) {
			printf("serve %d aborted reading errno=%s\n", number, n == 0 ? "eof" : errno_name(errno));
			return 1;
		}
		used += (size_t)n;
		request[used] = 0;
		if (strstr(request, "\r\n\r\n") || strstr(request, "\n\n"))
			break;
	}
	file = open(path, O_RDONLY | O_CLOEXEC);
	if (file < 0 || sys_fstat(file, &status) != 0) {
		printf("serve %d cannot open %s\n", number, path);
		return 1;
	}
	snprintf(header, sizeof(header), "HTTP/1.0 200 OK\r\nContent-Length: %ld\r\nConnection: close\r\n\r\n", (long)status.st_size);
	if (write_full(fd, header, strlen(header)) != 0) {
		printf("serve %d aborted writing errno=%s\n", number, errno_name(errno));
		close(file);
		return 1;
	}
	while ((n = read(file, chunk, sizeof(chunk))) > 0) {
		if (write_full(fd, chunk, (size_t)n) != 0) {
			printf("serve %d aborted writing errno=%s after %ld bytes\n", number, errno_name(errno), sent);
			close(file);
			return 1;
		}
		sent += n;
	}
	close(file);
	sys_shutdown(fd, SHUT_WR);
	/* Wait for the client to close, so the close does not reset bytes
	 * still in flight. */
	while ((n = read(fd, chunk, sizeof(chunk))) > 0)
		;
	printf("serve %d sent %ld bytes\n", number, sent);
	return 0;
}

static int serve_mode(int argc, char **argv)
{
	pid_t children[64];
	int port, count, fd, accepted, i, ok = 0, status;

	if (argc != 5 || (port = parse_port(argv[2])) < 0 || (count = atoi(argv[4])) <= 0 || count > 64) {
		printf("usage: netprobe serve PORT FILE COUNT\n");
		return 2;
	}
	fd = sys_socket(AF_INET, SOCK_STREAM | SOCK_CLOEXEC, 0);
	if (fd < 0 || set_int_option(fd, SOL_SOCKET, SO_REUSEADDR, 1) != 0 || bind4(fd, INADDR_ANY, (uint16_t)port) != 0 ||
	    sys_listen(fd, 16) != 0) {
		printf("serve failed: listen on %d errno=%s\n", port, errno_name(errno));
		return 1;
	}
	printf("serve listening %d\n", port);
	fflush(stdout);
	for (i = 0; i < count; i++) {
		accepted = sys_accept4(fd, NULL, NULL, SOCK_CLOEXEC);
		if (accepted < 0) {
			if (errno == EINTR) {
				i--;
				continue;
			}
			printf("serve failed: accept errno=%s\n", errno_name(errno));
			return 1;
		}
		children[i] = spawn();
		if (children[i] == 0) {
			close(fd);
			_exit(serve_one(accepted, argv[3], i + 1));
		}
		close(accepted);
		if (children[i] < 0) {
			printf("serve failed: fork errno=%s\n", errno_name(errno));
			return 1;
		}
	}
	close(fd);
	for (i = 0; i < count; i++) {
		status = child_status(children[i]);
		if (WIFEXITED(status) && WEXITSTATUS(status) == 0)
			ok++;
	}
	printf("serve done %d of %d\n", ok, count);
	return 0;
}

/* --- service faults ---------------------------------------------------- */

static void watchdog_note(int number)
{
	(void)number;
}

/* hold keeps a TCP connection to an echo service, a connected UDP socket,
 * and a listener while the caller kills a service. With "fail" it blocks
 * in read and expects the read to fail, then checks that the old sockets
 * stay failed and that new ones work. With "survive" it waits SECONDS and
 * expects the connection to carry an echo afterwards. */
static int hold_mode(int argc, char **argv)
{
	struct timeval timeout = {10, 0};
	char buffer[64];
	const char *via;
	uint32_t address;
	int port, seconds, fail, tcp, udp, listening, attempt, fresh, events, saved;
	pid_t watchdog;
	long n, start;

	group = "hold";
	if (argc != 6 || (port = parse_port(argv[3])) < 0 || (seconds = atoi(argv[5])) <= 0 ||
	    (strcmp(argv[4], "fail") && strcmp(argv[4], "survive")) || resolve(argv[2], &address, &via) != 0) {
		printf("usage: netprobe hold HOST PORT fail|survive SECONDS\n");
		return 2;
	}
	fail = !strcmp(argv[4], "fail");
	tcp = sys_socket(AF_INET, SOCK_STREAM, 0);
	REQUIRE(tcp >= 0 && connect4(tcp, address, (uint16_t)port) == 0, "connect");
	REQUIRE(write(tcp, "before", 6) == 6 && read_full(tcp, buffer, 6) == 0, "echo before");
	udp = sys_socket(AF_INET, SOCK_DGRAM, 0);
	REQUIRE(udp >= 0 && connect4(udp, address, (uint16_t)port) == 0, "udp connect");
	listening = sys_socket(AF_INET, SOCK_STREAM | SOCK_NONBLOCK, 0);
	REQUIRE(listening >= 0 && bind4(listening, INADDR_ANY, 0) == 0 && sys_listen(listening, 4) == 0, "listener");
	printf("hold ready\n");
	fflush(stdout);

	if (!fail) {
		events = revents(tcp, POLLIN, seconds * 1000);
		if (events != 0) {
			/* The native stack drops the address with a dead driver and
			 * aborts what was bound to it; the Linux socket follows. It
			 * must fail with a network error, and new sockets must work
			 * once the driver is back. */
			n = read(tcp, buffer, sizeof(buffer));
			REQUIRE(n < 0 && (errno == ECONNRESET || errno == ENETRESET || errno == ECONNABORTED || errno == ENETDOWN ||
			                  errno == ETIMEDOUT), "a dropped connection fails with a network error");
			printf("hold reset read=%s\n", errno_name(errno));
			close(tcp);
			close(udp);
			close(listening);
			goto fresh;
		}
		sys_setsockopt(tcp, SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof(timeout));
		REQUIRE(write(tcp, "after", 5) == 5 && read_full(tcp, buffer, 5) == 0 && !memcmp(buffer, "after", 5), "echo after the fault");
		printf("hold survived\n");
		return 0;
	}

	/* A blocking read must end on its own. The watchdog child's signal
	 * only turns a hang into a reported failure. */
	handle(SIGUSR1, watchdog_note, 0);
	{
		pid_t parent = getpid();

		watchdog = spawn();
		if (watchdog == 0) {
			nap(seconds * 1000);
			kill(parent, SIGUSR1);
			_exit(0);
		}
	}
	start = now_ms();
	n = read(tcp, buffer, sizeof(buffer));
	saved = errno;
	if (watchdog > 0) {
		kill(watchdog, SIGKILL);
		child_status(watchdog);
	}
	errno = saved;
	if (n < 0 && errno == EINTR) {
		printf("hold failed: read still blocked after %d s\n", seconds);
		return 1;
	}
	printf("hold woke read=%s after=%s\n", n < 0 ? errno_name(errno) : n == 0 ? "eof" : "data",
	       now_ms() - start < seconds * 1000L ? "bounded" : "late");
	errno = 0;
	n = sys_send(tcp, "stale", 5, MSG_NOSIGNAL);
	printf("hold stale tcp=%s", n < 0 ? errno_name(errno) : "sent");
	errno = 0;
	n = sys_send(udp, "stale", 5, 0);
	if (n >= 0 && (revents(udp, POLLIN, 1000) & POLLIN))
		n = sys_recv(udp, buffer, sizeof(buffer), MSG_DONTWAIT) >= 0 ? 1 : -1;
	else if (n >= 0)
		n = 0;
	printf(" udp=%s", n < 0 ? errno_name(errno) : n == 1 ? "echoed" : "lost");
	errno = 0;
	n = sys_accept4(listening, NULL, NULL, 0);
	printf(" accept=%s\n", n < 0 ? errno_name(errno) : "accepted");
	close(tcp);
	close(udp);
	close(listening);

	/* New sockets in the same process work once the service is back. */
fresh:
	for (attempt = 1; attempt <= 30; attempt++) {
		fresh = sys_socket(AF_INET, SOCK_STREAM, 0);
		if (fresh >= 0) {
			sys_setsockopt(fresh, SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof(timeout));
			if (connect4(fresh, address, (uint16_t)port) == 0 && write(fresh, "fresh", 5) == 5 &&
			    read_full(fresh, buffer, 5) == 0 && !memcmp(buffer, "fresh", 5)) {
				close(fresh);
				printf("hold recovered attempt=%d\n", attempt);
				return 0;
			}
			close(fresh);
		}
		nap(1000);
	}
	printf("hold failed: no fresh connection\n");
	return 1;
}

/* --- bench -------------------------------------------------------------- */

static long long now_us(void)
{
	struct timespec time;

	clock_gettime(CLOCK_MONOTONIC, &time);
	return time.tv_sec * 1000000LL + time.tv_nsec / 1000LL;
}

static unsigned char bench_chunk[65536];

/* mebibytes reads a stream size argument, 1 to 4096 MiB, as bytes. */
static long mebibytes(const char *text)
{
	char *end;
	long value = strtol(text, &end, 10);

	return *end == 0 && value >= 1 && value <= 4096 ? value << 20 : -1;
}

static long long rate(long total, long long elapsed)
{
	return elapsed ? (long long)total * 1000000LL / elapsed / (1 << 20) : 0;
}

/* bench_stream moves total bytes from one process to another over a
 * loopback connection; the reader answers one byte at the end. */
static int bench_stream(long total)
{
	int client, server, status;
	long long start, elapsed;
	long moved, n;
	pid_t child;
	char byte = 'x';

	REQUIRE(tcp_pair(&client, &server) == 0, "pair");
	child = spawn();
	if (child == 0) {
		for (moved = 0; moved < total; moved += n)
			if ((n = read(server, bench_chunk, sizeof(bench_chunk))) <= 0)
				_exit(1);
		_exit(write(server, &byte, 1) == 1 ? 0 : 1);
	}
	REQUIRE(child > 0, "fork reader");
	start = now_us();
	for (moved = 0; moved < total; moved += n) {
		n = total - moved < (long)sizeof(bench_chunk) ? total - moved : (long)sizeof(bench_chunk);
		REQUIRE(write_full(client, bench_chunk, (size_t)n) == 0, "stream transfer");
	}
	REQUIRE(read(client, &byte, 1) == 1, "stream end");
	elapsed = now_us() - start;
	status = child_status(child);
	REQUIRE(WIFEXITED(status) && WEXITSTATUS(status) == 0, "reader exit");
	close_pair(client, server);
	printf("bench tcp-stream bytes=%ld us=%lld MiBps=%lld\n", total, elapsed, rate(total, elapsed));
	return 0;
}

/* bench_request_reply sends one byte to an echoing process and waits for it
 * to come back, rounds times. */
static int bench_request_reply(int rounds)
{
	int client, server, i;
	long long start, elapsed;
	pid_t child;
	char byte = 'x';

	REQUIRE(tcp_pair(&client, &server) == 0, "pair");
	child = spawn();
	if (child == 0) {
		for (i = 0; i < rounds; i++)
			if (read(server, &byte, 1) != 1 || write(server, &byte, 1) != 1)
				_exit(1);
		_exit(0);
	}
	REQUIRE(child > 0, "fork echo");
	start = now_us();
	for (i = 0; i < rounds; i++)
		REQUIRE(write(client, &byte, 1) == 1 && read(client, &byte, 1) == 1, "request and reply");
	elapsed = now_us() - start;
	child_status(child);
	close_pair(client, server);
	printf("bench tcp-request-reply rounds=%d us-per-round=%lld\n", rounds, elapsed / rounds);
	return 0;
}

/* bench_connect opens, connects, accepts and closes connects connections in
 * one process. It times the whole loop and each of the five calls. */
static int bench_connect(int connects)
{
	long long start, mark, elapsed, spent[5] = {0, 0, 0, 0, 0};
	int fd, client, server, i;
	uint16_t port;

	fd = listener(&port, 0);
	REQUIRE(fd >= 0, "listener");
	start = now_us();
	for (i = 0; i < connects; i++) {
		mark = now_us();
		client = sys_socket(AF_INET, SOCK_STREAM, 0);
		REQUIRE(client >= 0, "socket");
		spent[0] += now_us() - mark;
		mark = now_us();
		REQUIRE(connect4(client, LOOPBACK, port) == 0, "connect");
		spent[1] += now_us() - mark;
		mark = now_us();
		server = sys_accept4(fd, NULL, NULL, 0);
		REQUIRE(server >= 0, "accept");
		spent[2] += now_us() - mark;
		mark = now_us();
		close(client);
		spent[3] += now_us() - mark;
		mark = now_us();
		close(server);
		spent[4] += now_us() - mark;
	}
	elapsed = now_us() - start;
	close(fd);
	printf("bench tcp-connect-close count=%d us-per-connection=%lld\n", connects, elapsed / connects);
	printf("bench tcp-socket count=%d us-per-op=%lld\n", connects, spent[0] / connects);
	printf("bench tcp-connect count=%d us-per-op=%lld\n", connects, spent[1] / connects);
	printf("bench tcp-accept count=%d us-per-op=%lld\n", connects, spent[2] / connects);
	printf("bench tcp-close-client count=%d us-per-op=%lld\n", connects, spent[3] / connects);
	printf("bench tcp-close-server count=%d us-per-op=%lld\n", connects, spent[4] / connects);
	return 0;
}

static int bench_udp(int rounds)
{
	long long start, elapsed;
	int udp, peer, i;
	pid_t child;

	udp = sys_socket(AF_INET, SOCK_DGRAM, 0);
	peer = sys_socket(AF_INET, SOCK_DGRAM, 0);
	REQUIRE(udp >= 0 && peer >= 0 && bind4(udp, LOOPBACK, 0) == 0 && bind4(peer, LOOPBACK, 0) == 0, "udp sockets");
	REQUIRE(connect4(udp, LOOPBACK, (uint16_t)local_port(peer)) == 0 && connect4(peer, LOOPBACK, (uint16_t)local_port(udp)) == 0, "udp peers");
	child = spawn();
	if (child == 0) {
		for (i = 0; i < rounds; i++)
			if (read(peer, bench_chunk, 64) <= 0 || write(peer, bench_chunk, 64) != 64)
				_exit(1);
		_exit(0);
	}
	REQUIRE(child > 0, "fork udp echo");
	start = now_us();
	for (i = 0; i < rounds; i++)
		REQUIRE(write(udp, bench_chunk, 64) == 64 && read(udp, bench_chunk, 64) == 64, "udp request and reply");
	elapsed = now_us() - start;
	child_status(child);
	printf("bench udp-request-reply rounds=%d us-per-round=%lld\n", rounds, elapsed / rounds);
	close(udp);
	close(peer);
	return 0;
}

/* bench_poll times a child's write until poll returns in the parent. */
static int bench_poll(int count)
{
	long long start, elapsed = 0;
	int client, server, i;
	pid_t child;
	char byte = 'x';

	REQUIRE(tcp_pair(&client, &server) == 0, "poll pair");
	for (i = 0; i < count; i++) {
		long long sent = 0;
		int link[2];

		REQUIRE(pipe(link) == 0, "pipe");
		child = spawn();
		if (child == 0) {
			long long stamp;

			nap(2);
			stamp = now_us();
			_exit(write(link[1], &stamp, sizeof(stamp)) == sizeof(stamp) && write(client, &byte, 1) == 1 ? 0 : 1);
		}
		REQUIRE(child > 0, "fork waker");
		REQUIRE(revents(server, POLLIN, 5000) & POLLIN, "poll wakes");
		start = now_us();
		REQUIRE(read(link[0], &sent, sizeof(sent)) == sizeof(sent) && read(server, &byte, 1) == 1, "wake stamp");
		elapsed += start - sent;
		child_status(child);
		close(link[0]);
		close(link[1]);
	}
	close_pair(client, server);
	printf("bench poll-wake count=%d us-per-wake=%lld\n", count, elapsed / count);
	return 0;
}

/* bench measures loopback sockets: stream throughput between two processes,
 * one-byte request and reply, connect and close with each call timed apart,
 * UDP request and reply, and the time from a write to a poll waiter's
 * return. It prints numbers, not verdicts; the lab records them. bench-stream
 * and bench-rr run one measurement alone, for a diagnostic counter pass. */
static int bench_mode(int argc, char **argv)
{
	long total = 32L << 20;
	int rounds;

	group = "bench";
	if (!strcmp(argv[1], "bench-rr")) {
		rounds = argc == 3 ? atoi(argv[2]) : 0;
		if (rounds <= 0 || rounds > 1000000) {
			printf("usage: netprobe bench-rr ROUNDS\n");
			return 2;
		}
		if (bench_request_reply(rounds) != 0)
			return 1;
		printf("bench ok\n");
		return 0;
	}
	if (argc == 3 && (total = mebibytes(argv[2])) < 0) {
		printf("usage: netprobe bench|bench-stream [MiB]\n");
		return 2;
	}
	if (!strcmp(argv[1], "bench-stream")) {
		if (bench_stream(total) != 0)
			return 1;
		printf("bench ok\n");
		return 0;
	}
	if (bench_stream(total) != 0 || bench_request_reply(2000) != 0 || bench_connect(200) != 0 ||
	    bench_udp(2000) != 0 || bench_poll(200) != 0)
		return 1;
	printf("bench ok\n");
	return 0;
}

/* stress_stream runs the loopback stream count times on fresh connections
 * and prints a line after each, so a watchdog sees which one stopped. */
static int stress_stream(long total, long count)
{
	long i;

	group = "stress";
	for (i = 1; i <= count; i++) {
		if (bench_stream(total) != 0) {
			printf("stress %ld failed\n", i);
			return 1;
		}
		printf("stress %ld ok\n", i);
	}
	printf("stress done\n");
	return 0;
}

/* long_stream carries total bytes over one connection, past the 32-bit
 * stream offsets of the native protocol and many times around every ring.
 * The reader checks each byte against the stream position. */
static int long_stream(long total)
{
	int client, server, status;
	long moved, n, i, mark;
	pid_t child;
	char byte = 'x';

	group = "long";
	REQUIRE(tcp_pair(&client, &server) == 0, "pair");
	child = spawn();
	if (child == 0) {
		for (moved = 0; moved < total; moved += n) {
			if ((n = read(server, bench_chunk, sizeof(bench_chunk))) <= 0)
				_exit(1);
			for (i = 0; i < n; i++)
				if (bench_chunk[i] != pattern_byte((size_t)(moved + i)))
					_exit(2);
		}
		_exit(write(server, &byte, 1) == 1 ? 0 : 1);
	}
	REQUIRE(child > 0, "fork reader");
	mark = 256L << 20;
	for (moved = 0; moved < total; moved += n) {
		n = total - moved < (long)sizeof(bench_chunk) ? total - moved : (long)sizeof(bench_chunk);
		for (i = 0; i < n; i++)
			bench_chunk[i] = pattern_byte((size_t)(moved + i));
		REQUIRE(write_full(client, bench_chunk, (size_t)n) == 0, "long transfer");
		if (moved + n >= mark) {
			printf("long MiB=%ld\n", (moved + n) >> 20);
			mark += 256L << 20;
		}
	}
	REQUIRE(read(client, &byte, 1) == 1, "long end");
	status = child_status(child);
	REQUIRE(WIFEXITED(status) && WEXITSTATUS(status) == 0, "reader saw every byte");
	close_pair(client, server);
	printf("long ok bytes=%ld\n", total);
	return 0;
}

/* --- remote bench ------------------------------------------------------- */

/* The remote peer (tools/pvelab/benchpeer.py) reads a nine-byte command on
 * each TCP connection: an opcode and a little-endian 64-bit count. 'S' reads
 * count bytes and answers one byte, 'G' sends count bytes, 'E' echoes count
 * single bytes. Its UDP socket on the same port echoes each datagram. */
static int peer_command(int fd, char opcode, unsigned long long count)
{
	unsigned char command[9];
	int i;

	command[0] = (unsigned char)opcode;
	for (i = 0; i < 8; i++)
		command[1 + i] = (unsigned char)(count >> (8 * i));
	return write_full(fd, command, sizeof(command));
}

static int peer_connect(uint32_t address, int port)
{
	int fd = sys_socket(AF_INET, SOCK_STREAM, 0);

	if (fd < 0)
		return -1;
	set_int_option(fd, IPPROTO_TCP, TCP_NODELAY, 1);
	if (connect4(fd, address, (uint16_t)port) != 0) {
		close(fd);
		return -1;
	}
	return fd;
}

/* remote_mode measures against a peer outside the guest: a stream to it, a
 * stream from it, one-byte TCP request and reply, and 64-byte UDP request
 * and reply. A UDP round with no answer within 200 ms counts as lost and
 * goes again; the time spent waiting for it leaves the result. */
static int remote_mode(int argc, char **argv)
{
	const int rounds = 2000;
	struct timeval timeout = {0, 200000};
	long long start, elapsed, waited = 0;
	uint32_t address;
	long total = 32L << 20, moved, n;
	int port, fd, i, lost = 0;
	char byte = 'x';

	group = "remote";
	if (argc < 4 || argc > 5 || parse_ipv4(argv[2], &address) != 0 || (port = parse_port(argv[3])) < 0 ||
	    (argc == 5 && (total = mebibytes(argv[4])) < 0)) {
		printf("usage: netprobe remote ADDRESS PORT [MiB]\n");
		return 2;
	}

	fd = peer_connect(address, port);
	REQUIRE(fd >= 0, "connect sink");
	start = now_us();
	REQUIRE(peer_command(fd, 'S', (unsigned long long)total) == 0, "sink command");
	for (moved = 0; moved < total; moved += n) {
		n = total - moved < (long)sizeof(bench_chunk) ? total - moved : (long)sizeof(bench_chunk);
		REQUIRE(write_full(fd, bench_chunk, (size_t)n) == 0, "stream to peer");
	}
	REQUIRE(read(fd, &byte, 1) == 1, "sink answer");
	elapsed = now_us() - start;
	close(fd);
	printf("bench remote-tcp-stream-tx bytes=%ld us=%lld MiBps=%lld\n", total, elapsed, rate(total, elapsed));

	fd = peer_connect(address, port);
	REQUIRE(fd >= 0, "connect source");
	start = now_us();
	REQUIRE(peer_command(fd, 'G', (unsigned long long)total) == 0, "source command");
	for (moved = 0; moved < total; moved += n)
		REQUIRE((n = read(fd, bench_chunk, sizeof(bench_chunk))) > 0, "stream from peer");
	elapsed = now_us() - start;
	close(fd);
	printf("bench remote-tcp-stream-rx bytes=%ld us=%lld MiBps=%lld\n", total, elapsed, rate(total, elapsed));

	fd = peer_connect(address, port);
	REQUIRE(fd >= 0, "connect echo");
	REQUIRE(peer_command(fd, 'E', (unsigned long long)rounds) == 0, "echo command");
	start = now_us();
	for (i = 0; i < rounds; i++)
		REQUIRE(write(fd, &byte, 1) == 1 && read(fd, &byte, 1) == 1, "request and reply");
	elapsed = now_us() - start;
	close(fd);
	printf("bench remote-tcp-request-reply rounds=%d us-per-round=%lld\n", rounds, elapsed / rounds);

	fd = sys_socket(AF_INET, SOCK_DGRAM, 0);
	REQUIRE(fd >= 0 && connect4(fd, address, (uint16_t)port) == 0, "udp socket");
	REQUIRE(sys_setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof(timeout)) == 0, "udp timeout");
	start = now_us();
	for (i = 0; i < rounds; i++) {
		uint32_t sequence = (uint32_t)i, echoed;
		long long sent;

		memcpy(bench_chunk, &sequence, sizeof(sequence));
		sent = now_us();
		REQUIRE(write(fd, bench_chunk, 64) == 64, "udp request");
		for (;;) {
			n = read(fd, bench_chunk, 64);
			if (n < 0 && (errno == EAGAIN || errno == EWOULDBLOCK)) {
				lost++;
				waited += now_us() - sent;
				REQUIRE(lost <= 20, "udp replies lost");
				i--;
				break;
			}
			REQUIRE(n == 64, "udp reply");
			memcpy(&echoed, bench_chunk, sizeof(echoed));
			if (echoed == sequence)
				break;
		}
	}
	elapsed = now_us() - start - waited;
	close(fd);
	printf("bench remote-udp-request-reply rounds=%d lost=%d us-per-round=%lld\n", rounds, lost, elapsed / rounds);
	printf("bench remote ok\n");
	return 0;
}

/* remote_serve_mode answers count connections from a remote client, one at a
 * time: accept, read one byte, write it back, wait for the client's close,
 * close. The client closing first keeps TIME_WAIT on its side, so the
 * guest's TIME_WAIT budget stays for the loopback benchmark. A watchdog
 * child kills the server after 90 seconds in case the client never comes. */
static int remote_serve_mode(int argc, char **argv)
{
	int port, count, fd, accepted, i;
	pid_t parent = getpid(), watchdog;
	char byte;

	if (argc != 4 || (port = parse_port(argv[2])) < 0 || (count = atoi(argv[3])) <= 0) {
		printf("usage: netprobe remote-serve PORT COUNT\n");
		return 2;
	}
	group = "remote-serve";
	fd = sys_socket(AF_INET, SOCK_STREAM | SOCK_CLOEXEC, 0);
	REQUIRE(fd >= 0 && set_int_option(fd, SOL_SOCKET, SO_REUSEADDR, 1) == 0 && bind4(fd, INADDR_ANY, (uint16_t)port) == 0 &&
	            sys_listen(fd, 16) == 0,
	        "listen");
	watchdog = spawn();
	if (watchdog == 0) {
		nap(90000);
		kill(parent, SIGKILL);
		_exit(0);
	}
	printf("remote-serve listening %d\n", port);
	fflush(stdout);
	for (i = 0; i < count; i++) {
		accepted = sys_accept4(fd, NULL, NULL, SOCK_CLOEXEC);
		if (accepted < 0 && errno == EINTR) {
			i--;
			continue;
		}
		REQUIRE(accepted >= 0, "accept");
		set_int_option(accepted, IPPROTO_TCP, TCP_NODELAY, 1);
		REQUIRE(read(accepted, &byte, 1) == 1 && write(accepted, &byte, 1) == 1, "request and reply");
		REQUIRE(read(accepted, &byte, 1) == 0, "client close");
		close(accepted);
	}
	close(fd);
	if (watchdog > 0) {
		kill(watchdog, SIGKILL);
		child_status(watchdog);
	}
	printf("remote-serve done %d\n", count);
	return 0;
}

/* --- main -------------------------------------------------------------- */

int main(int argc, char **argv)
{
	size_t i;
	int status = 0;

	setvbuf(stdout, NULL, _IOLBF, 0);
	trace = getenv("NETPROBE_TRACE") != NULL;
	if (argc > 0 && argv[0] && strchr(argv[0], '/'))
		self = argv[0];
	signal(SIGPIPE, SIG_IGN);
	if (argc == 4 && !strcmp(argv[1], "exec-check"))
		return exec_check(atoi(argv[2]), atoi(argv[3]));
	if (argc == 3 && !strcmp(argv[1], "resolve"))
		return resolve_mode(argv[2]);
	if (argc > 1 && !strcmp(argv[1], "fetch"))
		return fetch_mode(argc, argv);
	if (argc > 1 && !strcmp(argv[1], "connect"))
		return connect_mode(argc, argv);
	if (argc > 1 && !strcmp(argv[1], "reset"))
		return reset_mode(argc, argv);
	if (argc > 1 && !strcmp(argv[1], "silent"))
		return silent_mode(argc, argv);
	if (argc > 1 && !strcmp(argv[1], "udp-echo"))
		return udp_echo_mode(argc, argv);
	if (argc > 1 && !strcmp(argv[1], "inherit"))
		return inherit_mode(argc, argv);
	if (argc > 1 && !strcmp(argv[1], "serve"))
		return serve_mode(argc, argv);
	if (argc == 4 && !strcmp(argv[1], "stress-stream"))
		return stress_stream(mebibytes(argv[2]), atol(argv[3]));
	if (argc == 3 && !strcmp(argv[1], "long-stream"))
		return long_stream(mebibytes(argv[2]));
	if ((argc == 2 || argc == 3) && (!strcmp(argv[1], "bench") || !strcmp(argv[1], "bench-stream") || !strcmp(argv[1], "bench-rr")))
		return bench_mode(argc, argv);
	if (argc > 1 && !strcmp(argv[1], "remote"))
		return remote_mode(argc, argv);
	if (argc > 1 && !strcmp(argv[1], "remote-serve"))
		return remote_serve_mode(argc, argv);
	if (argc > 1 && !strcmp(argv[1], "hold"))
		return hold_mode(argc, argv);
	if (argc > 2 || (argc == 2 && strcmp(argv[1], "all"))) {
		for (i = 0; i < sizeof(groups) / sizeof(groups[0]); i++)
			if (argc == 2 && !strcmp(argv[1], groups[i].name))
				return run_group(&groups[i]);
		printf("usage: netprobe [all|GROUP|resolve|fetch|connect|reset|silent|udp-echo|inherit|serve|hold|bench|remote|remote-serve] ...\n");
		return 2;
	}
	for (i = 0; i < sizeof(groups) / sizeof(groups[0]); i++)
		status |= run_group(&groups[i]);
	printf(status ? "netprobe failed\n" : "netprobe ok\n");
	return status;
}
