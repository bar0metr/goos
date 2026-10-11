package main

import "golang.org/x/sys/unix"

func makeHostRaw(state *unix.Termios) {
	state.Iflag &^= unix.BRKINT | unix.ICRNL | unix.INPCK | unix.ISTRIP | unix.IXON
	state.Oflag &^= unix.OPOST
	state.Cflag &^= unix.CSIZE | unix.PARENB
	state.Cflag |= unix.CS8
	state.Lflag &^= unix.ECHO | unix.ICANON | unix.IEXTEN | unix.ISIG
	state.Cc[unix.VMIN] = 1
	state.Cc[unix.VTIME] = 0
}
