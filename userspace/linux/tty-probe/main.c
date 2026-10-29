/* tty-probe checks the Linux terminal of an interactive GoOS launch, one
 * contract at a time: descriptor identity, /dev/tty, termios, geometry,
 * sessions and process groups, stop and continue, and fsync. Run it from a
 * terminal: "tty-probe identity" prints "tty identity ok" or the first
 * check that failed. "tty-probe canon" and "tty-probe raw" read what the
 * caller types and print it back, for a harness that drives the keyboard.
 * The same binary runs on Linux, where the reference results come from. */
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <sys/wait.h>
#include <termios.h>
#include <unistd.h>

/* The bundled musl leaves out its termios and stat files; these are the
 * calls they make. */
static int get_settings(int fd, struct termios *settings)
{
	return ioctl(fd, TCGETS, settings) == 0 ? 0 : -1;
}

static int set_settings(int fd, int action, const struct termios *settings)
{
	return ioctl(fd, TCSETS + action, settings) == 0 ? 0 : -1;
}

static int file_status(int fd, struct stat *status)
{
	return syscall(SYS_fstat, fd, status) == 0 ? 0 : -1;
}

static int failed(const char *step)
{
	printf("tty identity failed: %s errno=%d\n", step, errno);
	return 1;
}

static int fails(long result, int expected)
{
	return result == -1 && errno == expected;
}

static volatile sig_atomic_t suspended;

/* note records SIGTSTP apart from other signals: their handlers may run in
 * either order. */
static void note(int signal)
{
	if (signal == SIGTSTP)
		suspended = 1;
}

static int descriptors(void)
{
	struct stat status;
	int fds[2], copy, tty;
	pid_t child;
	int code;

	if (!isatty(0) || !isatty(1) || !isatty(2))
		return failed("standard descriptors are terminals");
	if (file_status(0, &status) != 0 || !S_ISCHR(status.st_mode))
		return failed("fstat reports a character device");
	if (pipe(fds) != 0 || isatty(fds[0]) || isatty(fds[1]) || !fails(ioctl(fds[0], TIOCGPGRP, &code), ENOTTY))
		return failed("a pipe is no terminal");
	close(fds[0]);
	close(fds[1]);
	copy = dup(1);
	if (copy < 0 || !isatty(copy) || dup2(copy, 9) != 9 || !isatty(9) || close(9) != 0 || close(copy) != 0)
		return failed("dup and dup2 keep a terminal");
	if ((fcntl(0, F_GETFL) & O_ACCMODE) != O_RDWR)
		return failed("standard descriptors are read-write");
	tty = open("/dev/tty", O_RDWR);
	if (tty < 0 || !isatty(tty) || close(tty) != 0)
		return failed("/dev/tty opens with a controlling terminal");
	child = fork();
	if (child == 0)
		_exit(isatty(0) && isatty(1) ? 0 : 1);
	if (child < 0 || waitpid(child, &code, 0) != child || !WIFEXITED(code) || WEXITSTATUS(code) != 0)
		return failed("a forked child keeps the terminal");
	child = fork();
	if (child == 0) {
		/* A new session has no controlling terminal, though its
		 * descriptors still name the old one. */
		if (setsid() != getpid() || getsid(0) != getpid() || !isatty(0))
			_exit(1);
		errno = 0;
		_exit(fails(open("/dev/tty", O_RDWR), ENXIO) ? 0 : 2);
	}
	if (child < 0 || waitpid(child, &code, 0) != child || !WIFEXITED(code) || WEXITSTATUS(code) != 0)
		return failed("setsid leaves no controlling terminal");
	return 0;
}

static int settings(void)
{
	struct termios saved, changed, again;
	struct winsize window;

	if (get_settings(0, &saved) != 0)
		return failed("TCGETS");
	if (!(saved.c_lflag & ICANON) || !(saved.c_lflag & ECHO) || !(saved.c_lflag & ISIG) ||
	    !(saved.c_oflag & OPOST) || !(saved.c_oflag & ONLCR) || !(saved.c_iflag & ICRNL) ||
	    saved.c_cc[VINTR] != 3 || saved.c_cc[VEOF] != 4 || saved.c_cc[VERASE] != 0x7f ||
	    saved.c_cc[VSUSP] != 0x1a || saved.c_cc[VMIN] != 1 || saved.c_cc[VTIME] != 0)
		return failed("the initial state is canonical with echo and signals");
	changed = saved;
	changed.c_lflag &= ~(ICANON | ECHO);
	changed.c_cc[VMIN] = 1;
	changed.c_cc[VTIME] = 0;
	if (set_settings(0, TCSANOW, &changed) != 0 || get_settings(1, &again) != 0 ||
	    (again.c_lflag & (ICANON | ECHO)) != 0 || set_settings(0, TCSAFLUSH, &saved) != 0 ||
	    set_settings(0, TCSADRAIN, &saved) != 0 || get_settings(0, &again) != 0 || again.c_lflag != saved.c_lflag)
		return failed("TCSETS, TCSETSW, and TCSETSF");
	if (ioctl(1, TIOCGWINSZ, &window) != 0 || window.ws_row == 0 || window.ws_col == 0)
		return failed("TIOCGWINSZ");
	if (!fails(ioctl(0, TIOCGWINSZ, (void *)8), EFAULT) || !fails(ioctl(0, TCGETS, (void *)8), EFAULT))
		return failed("a bad pointer is EFAULT");
	if (!fails(ioctl(0, 0x5603, &window), ENOTTY))
		return failed("an unknown request is ENOTTY");
	return 0;
}

static int groups(void)
{
	pid_t self = getpid(), child, group;
	int code, go[2];
	char byte;

	if (getpgrp() != self || getpgid(0) != self || getsid(0) != self || tcgetpgrp(0) != self)
		return failed("an interactive launch leads its session and group");
	/* A shell ignores SIGTTOU, which a background tcsetpgrp would raise. */
	signal(SIGTTOU, SIG_IGN);
	if (!fails(setsid(), EPERM) || !fails(setpgid(0, 0), EPERM))
		return failed("a session leader stays where it is");
	if (kill(0, 0) != 0 || kill(-self, 0) != 0)
		return failed("signal 0 to the own group");
	if (pipe(go) != 0)
		return failed("pipe");
	child = fork();
	if (child == 0) {
		close(go[1]);
		setpgid(0, 0);
		read(go[0], &byte, 1);
		_exit(0);
	}
	if (child < 0 || (setpgid(child, child) != 0 && errno != EACCES))
		return failed("a parent moves its child to a new group");
	group = getpgid(child);
	if (group != child || getsid(child) != self)
		return failed("the child leads a group in the same session");
	if (tcsetpgrp(0, child) != 0 || tcgetpgrp(0) != child)
		return failed("TIOCSPGRP to a group of the session");
	if (tcsetpgrp(0, self) != 0 || tcgetpgrp(0) != self)
		return failed("TIOCSPGRP back to the shell's group");
	errno = 0;
	if (!fails(tcsetpgrp(0, 30000), ESRCH) && errno != EPERM)
		return failed("TIOCSPGRP to no group");
	if (!fails(setpgid(child, 30000), EPERM))
		return failed("setpgid to a group that does not exist");
	if (kill(-child, 0) != 0)
		return failed("signal 0 to the child's group");
	close(go[0]);
	close(go[1]);
	if (waitpid(child, &code, 0) != child || !WIFEXITED(code))
		return failed("the child ends");
	return 0;
}

static int jobs(void)
{
	struct sigaction action;
	pid_t child;
	int code, fds[2];
	char byte;

	/* The children lead groups of their own: the probe's own group has no
	 * parent in the session, so Linux discards a terminal stop sent to it. */
	child = fork();
	if (child == 0) {
		setpgid(0, 0);
		for (;;)
			pause();
	}
	setpgid(child, child);
	if (child < 0 || kill(child, SIGSTOP) != 0 ||
	    waitpid(child, &code, WUNTRACED) != child || !WIFSTOPPED(code) || WSTOPSIG(code) != SIGSTOP)
		return failed("SIGSTOP stops and WUNTRACED reports it");
	if (waitpid(child, &code, WUNTRACED | WNOHANG) != 0)
		return failed("a stop is reported once");
	if (kill(child, SIGCONT) != 0 || waitpid(child, &code, WCONTINUED) != child || !WIFCONTINUED(code))
		return failed("SIGCONT continues and WCONTINUED reports it");
	if (kill(child, SIGTSTP) != 0 || waitpid(child, &code, WUNTRACED) != child || !WIFSTOPPED(code) || WSTOPSIG(code) != SIGTSTP)
		return failed("SIGTSTP stops by default");
	if (kill(child, SIGKILL) != 0 || waitpid(child, &code, 0) != child || !WIFSIGNALED(code) || WTERMSIG(code) != SIGKILL)
		return failed("SIGKILL ends a stopped process");

	if (pipe(fds) != 0)
		return failed("pipe");
	child = fork();
	if (child == 0) {
		setpgid(0, 0);
		memset(&action, 0, sizeof action);
		action.sa_handler = note;
		sigaction(SIGTSTP, &action, NULL);
		sigaction(SIGWINCH, &action, NULL);
		write(fds[1], "r", 1);
		while (!suspended)
			pause();
		write(fds[1], "t", 1);
		_exit(0);
	}
	setpgid(child, child);
	close(fds[1]);
	if (child < 0 || read(fds[0], &byte, 1) != 1 || kill(child, SIGWINCH) != 0 || kill(child, SIGTSTP) != 0 ||
	    read(fds[0], &byte, 1) != 1 || byte != 't' || waitpid(child, &code, 0) != child || !WIFEXITED(code))
		return failed("a SIGTSTP handler runs instead of a stop");
	close(fds[0]);
	return 0;
}

/* A terminal stop sent to a group with no parent in the session, the
 * probe's own, is discarded; SIGSTOP is not. */
static int orphans(void)
{
	pid_t child = fork();
	int code;

	if (child == 0) {
		for (;;)
			pause();
	}
	if (child < 0 || kill(child, SIGTSTP) != 0 || waitpid(child, &code, WUNTRACED | WNOHANG) != 0)
		return failed("SIGTSTP to an orphaned group is discarded");
	kill(child, SIGKILL);
	waitpid(child, &code, 0);
	return 0;
}

static int durability(void)
{
	int fd = open("/tmp/tty-probe-sync", O_WRONLY | O_CREAT | O_TRUNC, 0644);
	int fds[2];

	if (fd < 0 || write(fd, "x", 1) != 1 || fsync(fd) != 0 || fdatasync(fd) != 0 || close(fd) != 0 ||
	    unlink("/tmp/tty-probe-sync") != 0)
		return failed("fsync of a regular file");
	if (!fails(fsync(0), EINVAL) || pipe(fds) != 0 || !fails(fsync(fds[0]), EINVAL) || !fails(fsync(42), EBADF))
		return failed("fsync of a terminal, a pipe, and nothing");
	close(fds[0]);
	close(fds[1]);
	return 0;
}

/* canon reads one line as the line discipline edits it. */
static int canon(void)
{
	char line[256];
	ssize_t count;

	printf("type a line\n");
	fflush(stdout);
	count = read(0, line, sizeof line - 1);
	if (count < 0)
		return failed("canonical read");
	line[count] = 0;
	printf("line=[%.*s] size=%d\n", count > 0 && line[count - 1] == '\n' ? (int)count - 1 : (int)count, line, (int)count);
	return 0;
}

/* raw reads three bytes with the discipline off, as an editor does. */
static int raw(void)
{
	struct termios saved, changed;
	unsigned char bytes[3];
	int got = 0;

	if (get_settings(0, &saved) != 0)
		return failed("TCGETS");
	changed = saved;
	changed.c_lflag &= ~(ICANON | ECHO | ISIG);
	changed.c_iflag &= ~ICRNL;
	changed.c_cc[VMIN] = 1;
	changed.c_cc[VTIME] = 0;
	if (set_settings(0, TCSAFLUSH, &changed) != 0)
		return failed("TCSETSF");
	printf("raw ready\n");
	fflush(stdout);
	while (got < 3) {
		ssize_t count = read(0, bytes + got, sizeof bytes - got);
		if (count <= 0)
			break;
		got += count;
	}
	set_settings(0, TCSANOW, &saved);
	printf("raw bytes %d: %02x %02x %02x\n", got, bytes[0], bytes[1], bytes[2]);
	return got != 3;
}

int main(int argc, char **argv)
{
	setvbuf(stdout, NULL, _IONBF, 0);
	if (argc > 1 && !strcmp(argv[1], "canon"))
		return canon();
	if (argc > 1 && !strcmp(argv[1], "raw"))
		return raw();
	if (argc > 1 && !strcmp(argv[1], "identity")) {
		static const struct {
			const char *name;
			int (*check)(void);
		} parts[] = {
			{"descriptors", descriptors}, {"settings", settings}, {"groups", groups},
			{"jobs", jobs}, {"orphans", orphans}, {"durability", durability},
		};
		for (size_t index = 0; index < sizeof parts / sizeof parts[0]; index++) {
			if (parts[index].check())
				return 1;
			printf("tty %s ok\n", parts[index].name);
		}
		printf("tty identity ok\n");
		return 0;
	}
	printf("usage: tty-probe identity|canon|raw\n");
	return 2;
}
