#!/bin/sh
set -eu

iso=${1:?ISO path required}
firmware=${2:?OVMF path required}
disk=${3:?test disk path required}
memory=${MEMORY:-512M}
# SMP is the guest's processor count, as a QEMU -smp argument.
smp=${SMP:-1}
log=build/logs/console-gui.log
dump=build/reports/console-gui.ppm
monitor=${TMPDIR:-/tmp}/goos-console-gui-monitor.$$
pid=

cleanup() {
	if [ -n "$pid" ]; then
		kill "$pid" 2>/dev/null || true
		wait "$pid" 2>/dev/null || true
	fi
	rm -f "$monitor"
}

fail() {
	printf 'FAIL console-gui: %s\n' "$1" >&2
	tail -n 40 "$log" >&2 2>/dev/null || true
	exit 1
}

monitor_command() {
	(printf '%s\n' "$1"; sleep 0.1) | nc -U "$monitor" >/dev/null 2>&1
}

send_key() {
	monitor_command "sendkey $1" || fail "QEMU rejected key $1"
}

wait_for_log() {
	pattern=$1
	description=$2
	limit=${3:-90}
	elapsed=0
	while ! grep -q "$pattern" "$log" 2>/dev/null; do
		kill -0 "$pid" 2>/dev/null || fail "QEMU exited before $description"
		[ "$elapsed" -lt "$limit" ] || fail "$description timeout"
		sleep 1
		elapsed=$((elapsed + 1))
	done
}

trap cleanup EXIT
trap 'cleanup; exit 130' HUP INT TERM
mkdir -p build/logs build/reports
rm -f "$log" "$dump" "$monitor"

qemu-system-x86_64 -machine q35 -smp "$smp" -accel tcg,thread=multi -m "$memory" \
	-drive "if=pflash,format=raw,readonly=on,file=$firmware" \
	-drive "if=none,id=goosdisk,format=raw,file=$disk" \
	-device virtio-blk-pci,drive=goosdisk,disable-modern=on \
	-device VGA -display none -serial "file:$log" \
	-monitor "unix:$monitor,server,nowait" -no-reboot -cdrom "$iso" &
pid=$!

elapsed=0
while [ ! -S "$monitor" ] && [ "$elapsed" -lt 100 ]; do
	kill -0 "$pid" 2>/dev/null || fail "QEMU exited before monitor startup"
	sleep 0.1
	elapsed=$((elapsed + 1))
done
[ -S "$monitor" ] || fail "QEMU monitor socket was not created"

wait_for_log 'FRAMEBUFFER_VALIDATION_PATTERN_OK' 'framebuffer marker'
wait_for_log 'FRAMEBUFFER_USER_ISOLATION_OK' 'framebuffer process-isolation marker'
wait_for_log '^goos> ' 'shell prompt' 180
wait_for_log '^BOOT_OK' 'completed validation boot' 180

# Validation output can scroll the overlay between BOOT_OK and the next kernel
# service pass. Sample the live display until that bounded pass restores it.
pixels_ok=false
attempt=0
while [ "$attempt" -lt 20 ]; do
	rm -f "$dump"
	monitor_command "screendump $dump" || fail "QEMU screendump command failed"
	elapsed=0
	while [ ! -s "$dump" ] && [ "$elapsed" -lt 50 ]; do
		sleep 0.1
		elapsed=$((elapsed + 1))
	done
	[ -s "$dump" ] || fail "QEMU did not create the screendump"
	if GOTOOLCHAIN=local GOCACHE="$(pwd)/build/cache/go" XDG_CACHE_HOME="$(pwd)/build/cache" \
		go run ./tools/ppmcheck "$dump" >/dev/null 2>&1; then
		pixels_ok=true
		break
	fi
	sleep 0.25
	attempt=$((attempt + 1))
done
if [ "$pixels_ok" != true ]; then
	GOTOOLCHAIN=local GOCACHE="$(pwd)/build/cache/go" XDG_CACHE_HOME="$(pwd)/build/cache" \
		go run ./tools/ppmcheck "$dump" || true
	fail "validation pixels did not match"
fi
printf 'PASS console-gui\n'

# Exercise editing and modifier paths before entering the mandatory command.
# COM1 is a file sink, so all input below can only arrive through IRQ1.
send_key shift-x
send_key backspace
for key in d i s c a r d; do
	send_key "$key"
done
send_key ctrl-u
send_key ctrl-l
send_key up
for key in e c h o spc g r a p h i c a l minus k e y b o a r d minus o k; do
	send_key "$key"
done
send_key ret

# The marker can follow the serial prompt on the same line: the shell is
# read from the volume (ADR 0175) and starts again after the boot's driver
# fault, so its prompt may be written just before it.
wait_for_log 'PS2_KEYBOARD_OK' 'PS/2 input marker'
wait_for_log '^graphical-keyboard-ok' 'graphical keyboard command output'
printf 'PASS console-keyboard\n'

# The file manager, driven only through IRQ1 (ADR 0124). Its screen is ANSI
# output mirrored to COM1, so the serial log shows what it drew; the markers
# it and the terminal write after raw mode ends prove the rest.
count_in_log() {
	grep -c "$1" "$log" 2>/dev/null || true
}

wait_for_count() {
	pattern=$1
	want=$2
	description=$3
	elapsed=0
	while [ "$(count_in_log "$pattern")" -lt "$want" ]; do
		kill -0 "$pid" 2>/dev/null || fail "QEMU exited before $description"
		[ "$elapsed" -lt 60 ] || fail "$description timeout"
		sleep 1
		elapsed=$((elapsed + 1))
	done
}

type_word() {
	for key in "$@"; do
		send_key "$key"
	done
}

type_word m o u n t ret
wait_for_log 'MOUNT_PROCESS_OK' 'mount command on framebuffer shell'
type_word d f ret
wait_for_log 'DF_PROCESS_OK' 'df command on framebuffer shell'
printf 'PASS console-filesystem-info\n'

type_word c d spc slash t m p ret
type_word g f m ret
wait_for_count 'F10.* Quit' 1 'file manager screen'
send_key f1
wait_for_log 'gfm keys' 'file manager help'
send_key esc
type_word tab down up home end pgdn pgup tab
send_key f7
wait_for_log 'Create directory' 'mkdir dialog'
type_word f m t e s t ret
wait_for_log 'Created "fmtest"' 'file manager mkdir'
monitor_command "screendump build/reports/gfm.ppm" || true
send_key f10
wait_for_log 'GFM_EXIT_OK' 'file manager exit'
wait_for_log 'GFM_RAW_INPUT_OK' 'raw keys reaching the file manager'
wait_for_log 'GFM_FUNCTION_KEYS_OK' 'function keys reaching the file manager'
wait_for_log 'GFM_MKDIR_OK' 'file manager mkdir marker'
wait_for_log 'TTY_RAW_MODE_OK' 'terminal leaving raw mode'
wait_for_count 'SHELL_TERMINAL_RESTORED_OK' 1 'shell restoring line mode'
type_word l s spc slash t m p ret
wait_for_log '^fmtest/' 'shell input after the file manager'
printf 'PASS console-file-manager\n'

# A file manager stopped with Ctrl-C leaves a usable terminal too.
type_word g f m ret
wait_for_count 'F10.* Quit' 2 'second file manager screen'
send_key ctrl-c
wait_for_count 'SHELL_TERMINAL_RESTORED_OK' 2 'line mode after Ctrl-C'
type_word e c h o spc a f t e r minus g f m minus o k ret
wait_for_log '^after-gfm-ok' 'shell input after a stopped file manager'
printf 'PASS console-file-manager-interrupt\n'

# The viewer uses the same framebuffer terminal session and returns it to the
# shell after navigation and a normal F10 exit.
type_word g v i e w spc slash s y s t e m slash m o t d ret
wait_for_log '/system/motd.*size' 'viewer screen on framebuffer shell'
type_word pgdn pgup right left home end
send_key f10
wait_for_log 'GVIEW_EXIT_OK' 'viewer exit'
wait_for_count 'SHELL_TERMINAL_RESTORED_OK' 3 'line mode after viewer'
type_word e c h o spc a f t e r minus g v i e w minus o k ret
wait_for_log '^after-gview-ok' 'shell input after viewer'
printf 'PASS console-viewer\n'

# gfm starts the viewer and the editor on the selected file through PID 1
# (ADR 0130); each takes the keys until it exits and gfm draws itself again.
# The edit is saved through a temporary file and a replacement (ADR 0129).
type_word m k d i r spc slash h o m e slash g v ret
type_word w r i t e spc slash h o m e slash g v slash n o t e spc h e l l o ret
type_word c d spc slash h o m e slash g v ret
managers=$(count_in_log 'F4.*Edit')
type_word g f m ret
wait_for_count 'F4.*Edit' $((managers + 1)) 'file manager with viewer and editor keys'
type_word home down f3
wait_for_log '/home/gv/note.*size' 'viewer started from the file manager'
views=$(count_in_log 'GVIEW_EXIT_OK')
type_word pgdn f10
wait_for_count 'GVIEW_EXIT_OK' $((views + 1)) 'viewer returning to the file manager'
editors=$(count_in_log 'F3 Save As')
send_key f4
wait_for_count 'F3 Save As' $((editors + 1)) 'editor started from the file manager'
type_word shift-x f2
wait_for_log 'Saved [0-9]* bytes' 'editor save'
managers=$(count_in_log 'F4.*Edit')
send_key f10
wait_for_log 'EDIT_EXIT_OK' 'editor exit'
wait_for_log 'EDIT_SAVE_OK' 'editor save marker'
wait_for_count 'F4.*Edit' $((managers + 1)) 'file manager repainted after the editor'
restored=$(count_in_log 'SHELL_TERMINAL_RESTORED_OK')
send_key f10
wait_for_log 'GFM_LAUNCH_OK' 'file manager launch marker'
wait_for_count 'SHELL_TERMINAL_RESTORED_OK' $((restored + 1)) 'line mode after the file manager'
type_word c a t spc slash h o m e slash g v slash n o t e ret
wait_for_log '^Xhello' 'edited file read back by the shell'
printf 'PASS console-nested-launch\n'

# A Linux program on the framebuffer terminal reads the PS/2 keys as bytes
# with its line discipline off, and the shell gets the terminal back after it
# (ADR 0206).
restored=$(count_in_log 'SHELL_TERMINAL_RESTORED_OK')
type_word slash s y s t e m slash l i n u x slash t t y minus p r o b e spc r a w ret
wait_for_log '^raw ready' 'Linux tty-probe on the framebuffer terminal'
type_word up
wait_for_log 'raw bytes 3: 1b 5b 41' 'arrow key bytes reaching the Linux program'
wait_for_count 'SHELL_TERMINAL_RESTORED_OK' $((restored + 1)) 'line mode after the Linux program'
type_word e c h o spc a f t e r minus l i n u x minus o k ret
wait_for_log '^after-linux-ok' 'shell input after the Linux program'
printf 'PASS console-linux-terminal\n'
