# Shared by tests/boot/memory.sh and tests/boot/allocator.sh: one round of
# shell work whose every step is checked before the round counts.
#
# The GoOS shell has no &&, no exit status a script can read, and runs each
# command of a ";" line whatever the previous one did, so a prompt says only
# that the line ended. Each step therefore runs on its own line, and its
# output must show that it did its work:
#
#   - no stage may print a diagnostic: utilities report "name: path: reason",
#     and the shell reports "exit status: N", "NAME: command not found", and
#     the kernel "user fault: ..." for a process it stopped;
#   - what the step made is then checked with another command: the size of a
#     copy and of a benchmark file with stat -b, a listed name with grep, the
#     bytes a pipeline wrote with wc -c, a Linux program's output with cat;
#   - cleanup must leave stat reporting the file gone.
#
# The sourcing script provides run CMD, which leaves the command's output in
# "$work/output", and fail MESSAGE, which must not return. leak_prepare reads
# the sizes the checks compare with; leak_round N runs round N.

# leak_names prints the command name of every stage of a pipeline.
leak_names() {
	printf '%s\n' "$1" | tr '|' '\n' | awk '{ n = $1; sub(/.*\//, "", n); if (n != "") print n }'
}

# leak_fail reports a failed step with the round, the command, and its output.
leak_fail() {
	fail "leak round $leak_round_number: $1: $2
$(sed -n '1,40p' "$work/output")"
}

# leak_step CMD [PATTERN...] runs CMD and requires that no stage reported a
# failure and that every PATTERN matches a line of its output.
leak_step() {
	command=$1
	shift
	run "$command"
	for name in $(leak_names "$command"); do
		grep -q "^$name: " "$work/output" && leak_fail "$command" "$name reported an error"
	done
	grep -q 'exit status\|command not found\|^user fault' "$work/output" && leak_fail "$command" 'a stage failed'
	for pattern in "$@"; do
		grep -q "$pattern" "$work/output" || leak_fail "$command" "no line matching $pattern"
	done
	return 0
}

# leak_size PATH BYTES requires PATH to hold BYTES bytes.
leak_size() {
	leak_step "stat -b $1" "^file $2 bytes\$"
}

# leak_remove PATH removes PATH and requires it gone.
leak_remove() {
	leak_step "rm $1"
	run "stat -b $1"
	grep -q "^stat: $1: no such file or directory\$" "$work/output" || leak_fail "rm $1" "$1 is still there"
}

# leak_value CMD PATTERN runs CMD and prints the first field of the line
# matching PATTERN.
leak_value() {
	leak_step "$1" "$2"
	grep "$2" "$work/output" | head -n 1 | awk '{ if ($1 == "file") print $2; else print $1 }'
}

# leak_prepare reads the sizes of the files the rounds copy and stream.
leak_prepare() {
	leak_round_number=0
	leak_copy_bytes=$(leak_value 'stat -b /system/bin/vmprobe' '^file [0-9]* bytes$')
	leak_motd_bytes=$(leak_value 'wc -c /system/motd' '^[0-9][0-9]*$')
	[ -n "$leak_copy_bytes" ] && [ "$leak_copy_bytes" -gt 0 ] && [ -n "$leak_motd_bytes" ] && [ "$leak_motd_bytes" -gt 0 ] ||
		fail "no sizes for the leak rounds: copy '$leak_copy_bytes', motd '$leak_motd_bytes'"
}

# leak_round N copies a large file, streams through the VFS, lists a
# directory into a file, runs a pipeline and a Linux program, and removes
# what each made, checking every step.
leak_round() {
	leak_round_number=$1
	leak_step 'cp /system/bin/vmprobe /tmp/leak.bin'
	leak_size /tmp/leak.bin "$leak_copy_bytes"
	leak_remove /tmp/leak.bin
	leak_step 'bench io /tmp/leak.io 256K' '^io write, 262144 bytes' '^io read, 262144 bytes'
	leak_size /tmp/leak.io 262144
	leak_remove /tmp/leak.io
	leak_step 'ls /system/bin > /tmp/leak.ls'
	leak_step 'grep vmprobe /tmp/leak.ls' '^vmprobe'
	leak_remove /tmp/leak.ls
	leak_step 'sleep 1 | cat /system/motd | write /tmp/leak.pipe'
	leak_step 'wc -c /tmp/leak.pipe' "^$leak_motd_bytes\$"
	leak_remove /tmp/leak.pipe
	leak_step '/system/linux/abi-probe random > /tmp/leak.r'
	leak_step 'cat /tmp/leak.r' '^random: available$'
	leak_remove /tmp/leak.r
}
