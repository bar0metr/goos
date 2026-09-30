package abi

import "testing"

func TestCanonicalLinuxErrnos(t *testing.T) {
	cases := map[Errno]uint64{
		EPERM: 1, ENOENT: 2, EIO: 5, E2BIG: 7, ENOEXEC: 8, EBADF: 9, EAGAIN: 11,
		ENOMEM: 12, EACCES: 13, EFAULT: 14, EBUSY: 16, EEXIST: 17,
		ENOTDIR: 20, EISDIR: 21, EINVAL: 22, ENFILE: 23, EMFILE: 24,
		ENOSPC: 28, EROFS: 30, EPIPE: 32, ENAMETOOLONG: 36,
		ENOSYS: 38, ENOTEMPTY: 39, EOPNOTSUPP: 95, ETIMEDOUT: 110,
		EUCLEAN: 117,
	}
	for errno, want := range cases {
		if uint64(errno) != want || int64(Negative(errno)) != -int64(want) {
			t.Fatalf("errno %d encoded as %d", want, int64(Negative(errno)))
		}
	}
}
