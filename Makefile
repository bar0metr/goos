SHELL := /bin/sh

BUILD_DIR := build
CACHE_DIR := $(BUILD_DIR)/cache
# GOTOOLCHAIN=local: the go command never downloads another toolchain; the
# pinned one (tools/toolchain.versions) is the one on PATH.
BUILD_ENV := GOTOOLCHAIN=local GOCACHE=$(abspath $(CACHE_DIR)/go) XDG_CACHE_HOME=$(abspath $(CACHE_DIR))
KERNEL_DIR = $(OUT_DIR)/kernel
KERNEL = $(KERNEL_DIR)/goos.efi
KERNEL_IO := $(BUILD_DIR)/kernel/io.o
INIT_IMAGE = $(OUT_DIR)/userspace/init
INIT_ELF = $(OUT_DIR)/userspace/init.elf
INIT_RUNTIME := $(BUILD_DIR)/userspace/runtime.o
SERVICE_IMAGE = $(OUT_DIR)/userspace/fault-probe
SHELL_IMAGE = $(OUT_DIR)/userspace/shell
SHELL_ELF = $(OUT_DIR)/userspace/shell.elf
VFS_IMAGE = $(OUT_DIR)/userspace/vfs
VFS_ELF = $(OUT_DIR)/userspace/vfs.elf
# Every directory under userspace/bin is one command in /system/bin. Nothing
# lists them: adding a directory adds the command, and the index PID 1 and the
# shell read is generated from the manifests the commands declare (ADR 0080).
# The boot archive carries the recovery subset tools/initramfs names
# (ADR 0175).
COMMANDS := $(sort $(notdir $(wildcard userspace/bin/*)))
COMMAND_DIR = $(OUT_DIR)/userspace/bin
COMMAND_ELF_DIR = $(OUT_DIR)/userspace/bin-elf
COMMAND_IMAGES = $(addprefix $(COMMAND_DIR)/,$(COMMANDS))
COMMAND_ELFS = $(addsuffix .elf,$(addprefix $(COMMAND_ELF_DIR)/,$(COMMANDS)))
COMMAND_INDEX = $(OUT_DIR)/userspace/commands
# Every command is rebuilt when any package a command imports changes; the
# commands share nearly all of them.
COMMAND_LIBRARY = $(call go_deps,./userspace/bin/...)
BLOCK_IMAGE = $(OUT_DIR)/userspace/block
BLOCK_ELF = $(OUT_DIR)/userspace/block.elf
VIRTIO_BLOCK_IMAGE = $(OUT_DIR)/userspace/virtio-blk
VIRTIO_BLOCK_ELF = $(OUT_DIR)/userspace/virtio-blk.elf
FAT32_IMAGE = $(OUT_DIR)/userspace/fat32
FAT32_ELF = $(OUT_DIR)/userspace/fat32.elf
LOG_IMAGE = $(OUT_DIR)/userspace/log
LOG_ELF = $(OUT_DIR)/userspace/log.elf
CONFIG_IMAGE = $(OUT_DIR)/userspace/config
CONFIG_ELF = $(OUT_DIR)/userspace/config.elf
TTY_IMAGE = $(OUT_DIR)/userspace/tty
TTY_ELF = $(OUT_DIR)/userspace/tty.elf
NETD_IMAGE = $(OUT_DIR)/userspace/netd
NETD_ELF = $(OUT_DIR)/userspace/netd.elf
VIRTIO_NET_IMAGE = $(OUT_DIR)/userspace/virtio-net
VIRTIO_NET_ELF = $(OUT_DIR)/userspace/virtio-net.elf
# tools/stackframes bounds every stack frame by the unmapped guard below the
# stack (ADR 0088). It runs on every userspace ELF before elf2native turns it
# into an image, and on the kernel right after it is linked.
STACKFRAMES_SOURCES := $(wildcard tools/stackframes/*.go) kernel/loader/native.go kernel/arch/amd64/stack.go
STACKFRAMES = $(BUILD_ENV) go run ./tools/stackframes
EFI_IMAGE = $(OUT_DIR)/iso/efi.img
VARIANT_ISO = $(OUT_DIR)/goos-x86_64.iso
ISO := $(BUILD_DIR)/goos-x86_64.iso
INITRAMFS = $(OUT_DIR)/initramfs/initramfs.tar
# RECOVERY=1 boots an ISO whose archive asks PID 1 for recovery mode, to
# diagnose or check a volume (ADR 0175). It is a separate archive and ISO in
# the same variant tree; nothing else is rebuilt for it.
RECOVERY ?= 0
ifeq ($(filter $(RECOVERY),0 1),)
$(error RECOVERY must be 0 or 1)
endif
RECOVERY_INITRAMFS = $(OUT_DIR)/initramfs/initramfs-recovery.tar
RECOVERY_EFI_IMAGE = $(OUT_DIR)/iso/efi-recovery.img
RECOVERY_ISO = $(OUT_DIR)/goos-x86_64-recovery.iso
RUN_ISO = $(if $(filter 1,$(RECOVERY)),$(RECOVERY_ISO),$(ISO))
# The installed system (ADR 0175): the tree tools/sysimage lays out from this
# build, and the volume made from it. DISK is the disk run and run-gui boot
# from. It is made from the system when it does not exist and never touched
# when it does; make disk-overwrite replaces it.
SYSTEM_TREE = $(OUT_DIR)/system
SYSTEM_IMAGE = $(OUT_DIR)/system.img
DISK ?= $(BUILD_DIR)/goos-disk.img
QEMU := qemu-system-x86_64
QEMU_SHARE ?= /opt/local/share/qemu
OVMF_CODE ?= $(QEMU_SHARE)/edk2-x86_64-code.fd
# Guest RAM for run and the QEMU tests, in QEMU -m syntax. q35 places RAM
# above 4 GiB once the guest has more than 2.75 GiB (ADR 0103).
MEMORY ?= 512M
# Cocoa is the native QEMU display backend on the primary macOS development
# host. Other hosts can select an available backend without changing targets,
# for example QEMU_DISPLAY=gtk or QEMU_DISPLAY=sdl.
QEMU_DISPLAY ?= cocoa
# NET=user attaches one legacy virtio-net function to QEMU user networking
# (ADR 0190, 0191); NET=none runs without a network. The function sits at
# slot 4, whose INTA the Q35 firmware routes to a line the disk does not use.
NET ?= user
ifeq ($(filter $(NET),user none),)
$(error NET must be user or none)
endif
NET_MAC ?= 52:54:00:12:34:56
QEMU_NET_FLAGS = $(if $(filter user,$(NET)),-netdev user,id=goosnet -device virtio-net-pci,netdev=goosnet,disable-modern=on,mac=$(NET_MAC),addr=0x4)
QEMU_COMMON_FLAGS = -machine q35 -m $(MEMORY) -drive if=pflash,format=raw,readonly=on,file=$(OVMF_CODE) -drive if=none,id=goosdisk,format=raw,file=$(DISK) -device virtio-blk-pci,drive=goosdisk,disable-modern=on $(QEMU_NET_FLAGS) -serial stdio -no-reboot
QEMU_FLAGS = $(QEMU_COMMON_FLAGS) -display none
QEMU_GUI_FLAGS = $(QEMU_COMMON_FLAGS) -device VGA -display $(QEMU_DISPLAY)
SOURCE_DATE_EPOCH ?= 0
HZ ?= 100
ifeq ($(filter $(HZ),100 250 1000),)
$(error HZ must be 100, 250, or 1000)
endif
BUILD_CONFIG := $(BUILD_DIR)/config/build
DIAG ?= 0
ifeq ($(filter $(DIAG),0 1),)
$(error DIAG must be 0 or 1)
endif
VALIDATION ?= 0
ifeq ($(filter $(VALIDATION),0 1),)
$(error VALIDATION must be 0 or 1)
endif
BUILD_TAGS := goos_hz$(HZ)
ifeq ($(DIAG),1)
BUILD_TAGS += goos_diag
endif
ifeq ($(VALIDATION),1)
BUILD_TAGS += goos_validation
endif
TINYGO_TAGS := -tags "$(BUILD_TAGS)"
# go_deps lists the Go and assembly files of every project package a userspace
# program imports, as go list sees them under the userspace target's tags, so
# editing any of them rebuilds the program. If go list fails the target is
# forced, and TinyGo reports the error.
USERSPACE_TAGS := uefi baremetal linux amd64 tinygo goos_userspace $(BUILD_TAGS)
go_deps = $(shell $(BUILD_ENV) GOOS=linux GOARCH=amd64 go list -tags "$(USERSPACE_TAGS)" -deps -f '{{if not .Standard}}{{$$d := .Dir}}{{range .GoFiles}}{{$$d}}/{{.}} {{end}}{{range .SFiles}}{{$$d}}/{{.}} {{end}}{{end}}' $(1) 2>/dev/null || echo FORCE)
# OPT is TinyGo's optimisation level for the kernel and every userspace image.
# TinyGo accepts 0, 1, 2, s, and z; 2 is its highest speed level (there is no 3).
# 2 is the default, measured faster than z (the smallest) in ADR 0090.
OPT ?= 2
ifeq ($(filter $(OPT),1 2 s z),)
$(error OPT must be 1, 2, s, or z)
endif
# The release comes from internal/version/version.go, the only place it is
# written; the kernel banner, uname, and system.build.release all read it there.
VERSION := $(shell sed -n 's/^[[:space:]]*Release[[:space:]]*=[[:space:]]*"\(.*\)"/\1/p' internal/version/version.go)
# Each configuration builds into its own tree, so switching HZ, DIAG,
# VALIDATION, or OPT never mixes objects built for another configuration. The
# level is always in the name, so a tree built before the default changed is
# never taken for one built at the new default.
OUT_DIR := $(BUILD_DIR)/variants/hz$(HZ)$(if $(filter 1,$(DIAG)),-diag)$(if $(filter 1,$(VALIDATION)),-validation)-opt$(OPT)

.DEFAULT_GOAL := all

FORCE:

.PHONY: FORCE all kernel userspace initramfs iso recovery-iso system-tree system-image disk disk-overwrite run-disk test-disk test-system-boot run run-gui run-debug release-check test test-all test-unit test-race test-integration test-security test-boot test-lscpu test-smp test-console-gui test-stress test-storage-stress test-fat32 test-fat32-stress test-partitions test-latency test-microbench test-performance test-reproducible lint fmt docs clean distclean toolchain-check version help linux-fixtures test-linux-reference test-linux-elf test-linux-abi test-linux-boot test-linux-musl test-linux-vfs test-linux-security test-linux-all test-linux-busybox test-linux-7zip profile-locks test-linux-bench test-linux-kernel-reference test-linux-process test-linux-exec test-linux-fork test-linux-pipe test-linux-signals test-linux-poll test-linux-shell test-linux-process-reference pve-doctor pve-artifacts test-pve-smoke test-pve-lscpu test-pve-smp test-pve-linux test-pve-7zip pve-run test-pvelab test-hardware

all: iso

kernel: $(KERNEL)


$(KERNEL_IO): kernel/arch/amd64/io.S
	@mkdir -p $(dir $@)
	clang -target x86_64-unknown-windows-gnu -ffreestanding -c $< -o $@

$(KERNEL): $(shell find kernel internal -type f) go.mod $(KERNEL_IO) $(STACKFRAMES_SOURCES)
	@mkdir -p $(KERNEL_DIR)
	$(BUILD_ENV) SOURCE_DATE_EPOCH=$(SOURCE_DATE_EPOCH) tinygo build $(TINYGO_TAGS) -target=tinygo/targets/goos-amd64.json -opt=$(OPT) -no-debug -o $@ ./kernel
	@$(STACKFRAMES) $@ || { rm -f $@; exit 1; }
	@nm -n $@ > $(KERNEL_DIR)/goos.sym 2>/dev/null || true

userspace: $(INIT_IMAGE) $(SERVICE_IMAGE) $(VFS_IMAGE) $(SHELL_IMAGE) $(COMMAND_IMAGES) $(COMMAND_INDEX) $(BLOCK_IMAGE) $(VIRTIO_BLOCK_IMAGE) $(LOG_IMAGE) $(CONFIG_IMAGE) $(TTY_IMAGE) $(FAT32_IMAGE) $(NETD_IMAGE) $(VIRTIO_NET_IMAGE)

$(INIT_RUNTIME): userspace/runtime/amd64.S
	@mkdir -p $(dir $@)
	clang -target x86_64-unknown-none-elf -ffreestanding -mno-red-zone -c $< -o $@

$(INIT_ELF): $(call go_deps,./userspace/cmd/init) tinygo/targets/goos-userspace-amd64.json userspace/runtime/amd64.ld $(INIT_RUNTIME)
	@mkdir -p $(OUT_DIR)/userspace
	$(BUILD_ENV) tinygo build $(TINYGO_TAGS) -target=tinygo/targets/goos-userspace-amd64.json -opt=$(OPT) -no-debug -o $@ ./userspace/cmd/init

$(INIT_IMAGE): $(INIT_ELF) tools/elf2native/main.go kernel/loader/native.go kernel/loader/native_encode.go $(STACKFRAMES_SOURCES)
	@$(STACKFRAMES) $(INIT_ELF)
	$(BUILD_ENV) go run ./tools/elf2native $(INIT_ELF) $@

$(SHELL_ELF): $(call go_deps,./userspace/cmd/shell) tinygo/targets/goos-userspace-amd64.json userspace/runtime/amd64.ld $(INIT_RUNTIME)
	@mkdir -p $(OUT_DIR)/userspace
	$(BUILD_ENV) tinygo build $(TINYGO_TAGS) -target=tinygo/targets/goos-userspace-amd64.json -opt=$(OPT) -no-debug -o $@ ./userspace/cmd/shell

$(SHELL_IMAGE): $(SHELL_ELF) tools/elf2native/main.go kernel/loader/native.go kernel/loader/native_encode.go $(STACKFRAMES_SOURCES)
	@$(STACKFRAMES) $(SHELL_ELF)
	$(BUILD_ENV) go run ./tools/elf2native $(SHELL_ELF) $@

$(VFS_ELF): $(call go_deps,./userspace/cmd/vfs) tinygo/targets/goos-userspace-amd64.json userspace/runtime/amd64.ld $(INIT_RUNTIME)
	@mkdir -p $(OUT_DIR)/userspace
	$(BUILD_ENV) tinygo build $(TINYGO_TAGS) -target=tinygo/targets/goos-userspace-amd64.json -opt=$(OPT) -no-debug -o $@ ./userspace/cmd/vfs

$(VFS_IMAGE): $(VFS_ELF) tools/elf2native/main.go kernel/loader/native.go kernel/loader/native_encode.go $(STACKFRAMES_SOURCES)
	@$(STACKFRAMES) $(VFS_ELF)
	$(BUILD_ENV) go run ./tools/elf2native $(VFS_ELF) $@

.SECONDEXPANSION:
$(COMMAND_ELFS): $(COMMAND_ELF_DIR)/%.elf: $$(wildcard userspace/bin/$$*/*.go) $(COMMAND_LIBRARY) tinygo/targets/goos-userspace-amd64.json userspace/runtime/amd64.ld $(INIT_RUNTIME)
	@mkdir -p $(COMMAND_ELF_DIR)
	$(BUILD_ENV) tinygo build $(TINYGO_TAGS) -target=tinygo/targets/goos-userspace-amd64.json -opt=$(OPT) -no-debug -o $@ ./userspace/bin/$*

$(COMMAND_IMAGES): $(COMMAND_DIR)/%: $(COMMAND_ELF_DIR)/%.elf tools/elf2native/main.go kernel/loader/native.go kernel/loader/native_encode.go $(STACKFRAMES_SOURCES)
	@mkdir -p $(COMMAND_DIR)
	@$(STACKFRAMES) $<
	$(BUILD_ENV) go run ./tools/elf2native $< $@

# The directory itself is a prerequisite: removing a command changes no source
# file, only the listing, and the index must drop it.
$(COMMAND_INDEX): userspace/bin $(wildcard userspace/bin/*/*.go) tools/commands/main.go userspace/commands/commands.go
	@mkdir -p $(dir $@)
	$(BUILD_ENV) go run ./tools/commands userspace/bin $@

$(BLOCK_ELF): $(call go_deps,./userspace/cmd/block) tinygo/targets/goos-userspace-amd64.json userspace/runtime/amd64.ld $(INIT_RUNTIME)
	@mkdir -p $(OUT_DIR)/userspace
	$(BUILD_ENV) tinygo build $(TINYGO_TAGS) -target=tinygo/targets/goos-userspace-amd64.json -opt=$(OPT) -no-debug -o $@ ./userspace/cmd/block

$(BLOCK_IMAGE): $(BLOCK_ELF) tools/elf2native/main.go kernel/loader/native.go kernel/loader/native_encode.go $(STACKFRAMES_SOURCES)
	@$(STACKFRAMES) $(BLOCK_ELF)
	$(BUILD_ENV) go run ./tools/elf2native $(BLOCK_ELF) $@

$(VIRTIO_BLOCK_ELF): $(call go_deps,./userspace/cmd/virtio-blk) tinygo/targets/goos-userspace-amd64.json userspace/runtime/amd64.ld $(INIT_RUNTIME)
	@mkdir -p $(OUT_DIR)/userspace
	$(BUILD_ENV) tinygo build $(TINYGO_TAGS) -target=tinygo/targets/goos-userspace-amd64.json -opt=$(OPT) -no-debug -o $@ ./userspace/cmd/virtio-blk

$(VIRTIO_BLOCK_IMAGE): $(VIRTIO_BLOCK_ELF) tools/elf2native/main.go kernel/loader/native.go kernel/loader/native_encode.go $(STACKFRAMES_SOURCES)
	@$(STACKFRAMES) $(VIRTIO_BLOCK_ELF)
	$(BUILD_ENV) go run ./tools/elf2native $(VIRTIO_BLOCK_ELF) $@

$(FAT32_ELF): $(call go_deps,./userspace/cmd/fat32) tinygo/targets/goos-userspace-amd64.json userspace/runtime/amd64.ld $(INIT_RUNTIME)
	@mkdir -p $(OUT_DIR)/userspace
	$(BUILD_ENV) tinygo build $(TINYGO_TAGS) -target=tinygo/targets/goos-userspace-amd64.json -opt=$(OPT) -no-debug -o $@ ./userspace/cmd/fat32

$(FAT32_IMAGE): $(FAT32_ELF) tools/elf2native/main.go kernel/loader/native.go kernel/loader/native_encode.go $(STACKFRAMES_SOURCES)
	@$(STACKFRAMES) $(FAT32_ELF)
	$(BUILD_ENV) go run ./tools/elf2native $(FAT32_ELF) $@

$(LOG_ELF): $(call go_deps,./userspace/cmd/logd) tinygo/targets/goos-userspace-amd64.json userspace/runtime/amd64.ld $(INIT_RUNTIME)
	@mkdir -p $(OUT_DIR)/userspace
	$(BUILD_ENV) tinygo build $(TINYGO_TAGS) -target=tinygo/targets/goos-userspace-amd64.json -opt=$(OPT) -no-debug -o $@ ./userspace/cmd/logd

$(LOG_IMAGE): $(LOG_ELF) tools/elf2native/main.go kernel/loader/native.go kernel/loader/native_encode.go $(STACKFRAMES_SOURCES)
	@$(STACKFRAMES) $(LOG_ELF)
	$(BUILD_ENV) go run ./tools/elf2native $(LOG_ELF) $@

$(CONFIG_ELF): $(call go_deps,./userspace/cmd/configd) tinygo/targets/goos-userspace-amd64.json userspace/runtime/amd64.ld $(INIT_RUNTIME)
	@mkdir -p $(OUT_DIR)/userspace
	$(BUILD_ENV) tinygo build $(TINYGO_TAGS) -target=tinygo/targets/goos-userspace-amd64.json -opt=$(OPT) -no-debug -o $@ ./userspace/cmd/configd

$(CONFIG_IMAGE): $(CONFIG_ELF) tools/elf2native/main.go kernel/loader/native.go kernel/loader/native_encode.go $(STACKFRAMES_SOURCES)
	@$(STACKFRAMES) $(CONFIG_ELF)
	$(BUILD_ENV) go run ./tools/elf2native $(CONFIG_ELF) $@

$(TTY_ELF): $(call go_deps,./userspace/cmd/tty) tinygo/targets/goos-userspace-amd64.json userspace/runtime/amd64.ld $(INIT_RUNTIME)
	@mkdir -p $(OUT_DIR)/userspace
	$(BUILD_ENV) tinygo build $(TINYGO_TAGS) -target=tinygo/targets/goos-userspace-amd64.json -opt=$(OPT) -no-debug -o $@ ./userspace/cmd/tty

$(TTY_IMAGE): $(TTY_ELF) tools/elf2native/main.go kernel/loader/native.go kernel/loader/native_encode.go $(STACKFRAMES_SOURCES)
	@$(STACKFRAMES) $(TTY_ELF)
	$(BUILD_ENV) go run ./tools/elf2native $(TTY_ELF) $@

$(NETD_ELF): $(call go_deps,./userspace/cmd/netd) tinygo/targets/goos-userspace-amd64.json userspace/runtime/amd64.ld $(INIT_RUNTIME)
	@mkdir -p $(OUT_DIR)/userspace
	$(BUILD_ENV) tinygo build $(TINYGO_TAGS) -target=tinygo/targets/goos-userspace-amd64.json -opt=$(OPT) -no-debug -o $@ ./userspace/cmd/netd

$(NETD_IMAGE): $(NETD_ELF) tools/elf2native/main.go kernel/loader/native.go kernel/loader/native_encode.go $(STACKFRAMES_SOURCES)
	@$(STACKFRAMES) $(NETD_ELF)
	$(BUILD_ENV) go run ./tools/elf2native $(NETD_ELF) $@

$(VIRTIO_NET_ELF): $(call go_deps,./userspace/cmd/virtio-net) tinygo/targets/goos-userspace-amd64.json userspace/runtime/amd64.ld $(INIT_RUNTIME)
	@mkdir -p $(OUT_DIR)/userspace
	$(BUILD_ENV) tinygo build $(TINYGO_TAGS) -target=tinygo/targets/goos-userspace-amd64.json -opt=$(OPT) -no-debug -o $@ ./userspace/cmd/virtio-net

$(VIRTIO_NET_IMAGE): $(VIRTIO_NET_ELF) tools/elf2native/main.go kernel/loader/native.go kernel/loader/native_encode.go $(STACKFRAMES_SOURCES)
	@$(STACKFRAMES) $(VIRTIO_NET_ELF)
	$(BUILD_ENV) go run ./tools/elf2native $(VIRTIO_NET_ELF) $@

$(SERVICE_IMAGE): tools/service-image/main.go kernel/loader/native.go kernel/loader/native_encode.go kernel/mm/vm/permissions.go
	@mkdir -p $(OUT_DIR)/userspace
	$(BUILD_ENV) go run ./tools/service-image $@

INITRAMFS_FLAGS = $(if $(filter 1,$(VALIDATION)),-validation)
INITRAMFS_INPUTS = $(INIT_IMAGE) $(SERVICE_IMAGE) $(VFS_IMAGE) $(SHELL_IMAGE) $(BLOCK_IMAGE) $(VIRTIO_BLOCK_IMAGE) $(TTY_IMAGE) $(FAT32_IMAGE) $(COMMAND_DIR) $(COMMAND_INDEX)

# The archive holds what boots before there is a volume and what recovery
# runs without one; the rest of the system is installed on the volume
# (ADR 0175). The recovery archive is the same with the request for recovery
# mode added.
initramfs: userspace
	@mkdir -p $(OUT_DIR)/initramfs/root/system
	@cp $(INIT_IMAGE) $(OUT_DIR)/initramfs/root/system/init
	@# A command whose directory was removed leaves its image behind; the
	@# archive takes only the commands the tree still has.
	@find $(COMMAND_DIR) -type f $(foreach command,$(COMMANDS),! -name $(command)) -delete
	$(BUILD_ENV) go run ./tools/initramfs $(INITRAMFS_FLAGS) $(INITRAMFS_INPUTS) $(INITRAMFS)
	@$(BUILD_ENV) go run ./tools/initramfs $(INITRAMFS_FLAGS) -force-recovery $(INITRAMFS_INPUTS) $(RECOVERY_INITRAMFS)

# The installed system as a directory tree and as a volume image. Both are
# made from this build's outputs alone, in name order, so the same build
# gives the same bytes. LINUX_EXTRA adds Linux programs under /system/linux.
system-tree: userspace
	@$(BUILD_ENV) LINUX_EXTRA=$(LINUX_EXTRA) go run ./tools/sysimage $(SHELL_IMAGE) $(CONFIG_IMAGE) $(LOG_IMAGE) $(NETD_IMAGE) $(VIRTIO_NET_IMAGE) $(COMMAND_DIR) $(COMMAND_INDEX) $(SYSTEM_TREE)

system-image: iso system-tree
	@rm -f $(SYSTEM_IMAGE)
	@$(BUILD_ENV) go run ./tools/mkfat32 -new -tree $(SYSTEM_TREE) $(SYSTEM_IMAGE)
	@printf 'built %s and %s (GoOS %s)\n' '$(ISO)' '$(SYSTEM_IMAGE)' '$(VERSION)'

# disk installs the system on a new DISK and refuses one that exists;
# disk-overwrite replaces it, data and all. run and run-gui make DISK only
# when it is missing, and never change one that is there.
disk: system-tree
	@$(BUILD_ENV) go run ./tools/mkfat32 -new -tree $(SYSTEM_TREE) $(DISK)
	@printf 'installed GoOS %s on %s\n' '$(VERSION)' '$(DISK)'

disk-overwrite: system-tree
	@rm -f $(DISK)
	@$(BUILD_ENV) go run ./tools/mkfat32 -new -tree $(SYSTEM_TREE) $(DISK)
	@printf 'installed GoOS %s on %s, replacing what was there\n' '$(VERSION)' '$(DISK)'

run-disk:
	@if [ -e '$(DISK)' ]; then printf 'using %s as it is\n' '$(DISK)'; else $(MAKE) --no-print-directory disk; fi

# A disposable installed disk for one QEMU test, made afresh every time.
test-disk: system-tree
	@test -n '$(TEST_DISK)' || { echo 'TEST_DISK is required' >&2; exit 2; }
	@rm -f $(TEST_DISK)
	@$(BUILD_ENV) go run ./tools/mkfat32 -new -tree $(SYSTEM_TREE) $(TEST_DISK)

# tools/mkiso writes both images from their inputs alone, with fixed names,
# serial number, and SOURCE_DATE_EPOCH timestamps, so a clean rebuild is
# byte-identical (ADR 0086).
$(EFI_IMAGE): $(KERNEL) initramfs $(wildcard tools/mkiso/*.go)
	@mkdir -p $(dir $@)
	@$(BUILD_ENV) SOURCE_DATE_EPOCH=$(SOURCE_DATE_EPOCH) go run ./tools/mkiso esp $(KERNEL) $(INITRAMFS) $@

iso: $(ISO)

$(VARIANT_ISO): $(EFI_IMAGE)
	@$(BUILD_ENV) SOURCE_DATE_EPOCH=$(SOURCE_DATE_EPOCH) go run ./tools/mkiso iso $(EFI_IMAGE) $(KERNEL) $(INITRAMFS) $@
	@test -s $@

recovery-iso: $(RECOVERY_ISO)

$(RECOVERY_EFI_IMAGE): $(KERNEL) initramfs $(wildcard tools/mkiso/*.go)
	@mkdir -p $(dir $@)
	@$(BUILD_ENV) SOURCE_DATE_EPOCH=$(SOURCE_DATE_EPOCH) go run ./tools/mkiso esp $(KERNEL) $(RECOVERY_INITRAMFS) $@

$(RECOVERY_ISO): $(RECOVERY_EFI_IMAGE)
	@$(BUILD_ENV) SOURCE_DATE_EPOCH=$(SOURCE_DATE_EPOCH) go run ./tools/mkiso iso $(RECOVERY_EFI_IMAGE) $(KERNEL) $(RECOVERY_INITRAMFS) $@
	@printf 'built %s (GoOS %s, recovery)\n' '$@' '$(VERSION)'

# build/goos-x86_64.iso is always the most recently requested configuration;
# build/config/build names it, and its variant directory, for the test harnesses.
$(ISO): $(VARIANT_ISO) FORCE
	@cmp -s $(VARIANT_ISO) $@ || cp $(VARIANT_ISO) $@
	@mkdir -p $(dir $(BUILD_CONFIG))
	@printf '%s\n' '$(HZ) $(DIAG) $(VALIDATION) $(OPT) $(notdir $(OUT_DIR))' >$(BUILD_CONFIG)
	@printf 'built %s (GoOS %s, %s)\n' '$@' '$(VERSION)' '$(notdir $(OUT_DIR))'

RUN_TARGET = $(if $(filter 1,$(RECOVERY)),recovery-iso,iso)

run: $(RUN_TARGET) run-disk
	$(QEMU) $(QEMU_FLAGS) -cdrom $(RUN_ISO)

run-gui: $(RUN_TARGET) run-disk
	$(QEMU) $(QEMU_GUI_FLAGS) -cdrom $(RUN_ISO)

run-debug: $(RUN_TARGET) run-disk
	$(QEMU) $(QEMU_FLAGS) -s -S -cdrom $(RUN_ISO)

test: test-unit test-integration test-security

test-all:
	$(MAKE) test
	$(MAKE) lint
	$(MAKE) docs
	$(MAKE) test-boot
	$(MAKE) test-lscpu
	$(MAKE) test-smp
	$(MAKE) test-system-boot
	$(MAKE) test-linux-boot
	$(MAKE) test-console-gui
	$(MAKE) test-stress STRESS_RUNS=2

# The release gate (0.1.1 specification §7). The reproducible-build test runs
# first: it cleans build/ and builds the ISO twice, which also proves that a
# clean build succeeds. Everything else then runs against that tree. Timing
# benchmarks are not a gate: they depend on the host (ADR 0085).
release-check:
	$(MAKE) test-reproducible
	$(MAKE) lint
	$(MAKE) docs
	$(MAKE) test
	$(MAKE) test-storage-stress
	$(MAKE) iso
	@tests/release/check.sh $(OUT_DIR) $(ISO)
	$(MAKE) test-boot
	$(MAKE) test-lscpu
	$(MAKE) test-smp
	$(MAKE) test-system-boot
	$(MAKE) test-linux-boot
	$(MAKE) test-console-gui
	$(MAKE) test-stress STRESS_RUNS=2
	@tests/release/check.sh $(OUT_DIR) $(ISO)

test-unit:
	$(BUILD_ENV) go test ./...
	$(BUILD_ENV) go test -tags goos_diag ./kernel/diag
	$(BUILD_ENV) go test -tags goos_hz1000 ./kernel/clock
	$(BUILD_ENV) go test -tags goos_validation ./kernel/lock

# The concurrency tests under the race detector (ADR 0187, ADR 0189): handle
# tables, channels, and the Linux tables shared the way processors share
# them, and the wait tables with the deadline index they keep.
test-race:
	$(BUILD_ENV) go test -race -run 'Race|Ways|Busy|Holder|TwoProcesses|ConcurrentFileIO' ./kernel/handle ./kernel/ipc ./kernel/compat/linux
	$(BUILD_ENV) go test -race ./kernel/deadline ./kernel/sched ./kernel/wait ./kernel/process

test-integration: kernel
	@tests/integration/kernel_artifact.sh $(KERNEL)

test-security:
	$(BUILD_ENV) go test ./internal/capability ./kernel/device ./kernel/executable ./kernel/handle ./kernel/initramfs ./kernel/ipc ./kernel/kcall ./kernel/loader ./kernel/mm/vm ./kernel/pci ./kernel/process ./kernel/ramfs ./kernel/shmem ./kernel/stream ./kernel/tty ./kernel/vfs ./userspace/block ./userspace/exec ./userspace/fat32 ./userspace/fsproto ./userspace/init ./userspace/partition ./userspace/runtime/sys ./userspace/shell ./userspace/startup ./userspace/vfs ./tools/stackframes

# The storage stress test (ADR 0102): the model-based FAT32 workload at a
# larger scale than the default suite, and the host FAT checker over volumes
# the filesystem built.
test-storage-stress: test-fat32-stress

# The FAT32 format (ADR 0102, ADR 0112): every geometry, Unicode names,
# timestamps, the corruption corpus, and interoperability with the host's FAT
# tools where they exist. The stress variant adds 128-512 KiB clusters, a
# 1 GiB and a 4 GiB - 1 file on a sparse host file, thousands of seeded
# corruptions, and a directory of 8000 long names.
test-fat32:
	$(BUILD_ENV) go test -count=1 ./userspace/fat32

test-partitions:
	$(BUILD_ENV) go test -count=1 ./userspace/partition ./userspace/block ./internal/imagefixture

test-fat32-stress:
	$(BUILD_ENV) GOOS_STORAGE_STRESS=1 go test -count=1 -timeout 60m ./userspace/fat32

test-boot:
	@$(MAKE) --no-print-directory iso VALIDATION=1
	@$(MAKE) --no-print-directory recovery-iso VALIDATION=1
	@$(MAKE) --no-print-directory test-disk VALIDATION=1 TEST_DISK=$(BUILD_DIR)/boot-test-virtio-blk.img
	@MEMORY=$(MEMORY) tests/boot/qemu_boot.sh $(ISO) $(OVMF_CODE) $(BUILD_DIR)/boot-test-virtio-blk.img

test-lscpu: iso
	@$(MAKE) --no-print-directory test-disk TEST_DISK=$(BUILD_DIR)/lscpu-test.img
	@MEMORY=$(MEMORY) tests/boot/lscpu.sh $(ISO) $(OVMF_CODE) $(BUILD_DIR)/lscpu-test.img

# SMP (docs/kernel/smp.md): one validation guest with four processors runs
# every processor at once. SMP_MATRIX=1 adds one-, two-, and eight-processor
# boots of the same scenario. The other boot targets take SMP=N as well.
SMP_MATRIX ?= 0

test-smp:
	@$(MAKE) --no-print-directory iso VALIDATION=1
	@$(MAKE) --no-print-directory test-disk VALIDATION=1 TEST_DISK=$(BUILD_DIR)/smp-test.img
	@MEMORY=$(MEMORY) tests/boot/smp.sh $(ISO) $(OVMF_CODE) $(BUILD_DIR)/smp-test.img
	@if [ "$(SMP_MATRIX)" = 1 ]; then \
		for cpus in 1 2 8; do \
			$(MAKE) --no-print-directory test-disk VALIDATION=1 TEST_DISK=$(BUILD_DIR)/smp-test.img && \
			MEMORY=$(MEMORY) SMP=$$cpus CPUS=$$cpus tests/boot/smp.sh $(ISO) $(OVMF_CODE) $(BUILD_DIR)/smp-test.img || exit 1; \
		done; \
	fi

# The installed system and recovery boot (ADR 0175): a normal boot from an
# installed disk, a reboot of it with storage faults, forced recovery, and
# recovery for a missing disk and each way the installed system can be unusable.
test-system-boot:
	@$(MAKE) --no-print-directory iso VALIDATION=1
	@$(MAKE) --no-print-directory recovery-iso system-tree VALIDATION=1
	@MEMORY=$(MEMORY) tests/boot/system.sh $(ISO) $(OVMF_CODE)

test-console-gui:
	@$(MAKE) --no-print-directory iso VALIDATION=1
	@$(MAKE) --no-print-directory test-disk VALIDATION=1 TEST_DISK=$(BUILD_DIR)/console-gui-virtio-blk.img
	@MEMORY=$(MEMORY) tests/boot/console_gui.sh $(ISO) $(OVMF_CODE) $(BUILD_DIR)/console-gui-virtio-blk.img

STRESS_RUNS ?= 4
STRESS_JOBS ?= 4
STRESS_ROUNDS ?= 15

test-stress: iso system-tree
	@MEMORY=$(MEMORY) SYSTEM_TREE=$(SYSTEM_TREE) tests/boot/stress_matrix.sh $(ISO) $(OVMF_CODE) $(STRESS_RUNS) $(STRESS_JOBS) $(STRESS_ROUNDS)

# Builds the ISO twice from a clean build/ and compares every artifact (ADR
# 0086). It cleans build/ itself, keeping only disks, logs, and reports.
test-reproducible:
	@MAKE='$(MAKE)' tests/build/reproducible.sh

LATENCY_REPEATS ?= 10

test-latency: iso
	@$(MAKE) --no-print-directory test-disk TEST_DISK=$(BUILD_DIR)/latency-virtio-blk.img
	@MEMORY=$(MEMORY) LATENCY_REPEATS=$(LATENCY_REPEATS) tests/boot/latency.sh $(ISO) $(OVMF_CODE) $(BUILD_DIR)/latency-virtio-blk.img

MICROBENCH_REPEATS ?= 5

# In-guest microbenchmarks (ADR 0085): context switch, IPC, and VFS reads
# measured in a booted system.
test-microbench: iso
	@$(MAKE) --no-print-directory test-disk TEST_DISK=$(BUILD_DIR)/microbench-virtio-blk.img
	@MEMORY=$(MEMORY) MICROBENCH_REPEATS=$(MICROBENCH_REPEATS) tests/boot/microbench.sh $(ISO) $(OVMF_CODE) $(BUILD_DIR)/microbench-virtio-blk.img

test-performance: test-microbench
	@mkdir -p $(BUILD_DIR)/reports
	@$(BUILD_ENV) go test -run '^$$' -bench BenchmarkAllocateFree -benchmem ./kernel/mm/pmm | tee $(BUILD_DIR)/reports/page-allocation.txt
	@$(BUILD_ENV) go test -run '^$$' -bench BenchmarkChannelPingPong -benchmem ./kernel/ipc | tee $(BUILD_DIR)/reports/ipc-ping-pong.txt
	@$(BUILD_ENV) go test -run '^$$' -bench BenchmarkSharedPagePayloadAccess -benchmem ./kernel/shmem | tee $(BUILD_DIR)/reports/shared-memory.txt
	@$(BUILD_ENV) go test -run '^$$' -bench BenchmarkStreamRoundTrip -benchmem ./kernel/stream | tee $(BUILD_DIR)/reports/stream-roundtrip.txt
	@$(BUILD_ENV) go test -run '^$$' -bench BenchmarkMetadataCreateUnlink -benchmem ./kernel/ramfs | tee $(BUILD_DIR)/reports/ramfs-metadata.txt
	@$(BUILD_ENV) go test -run '^$$' -bench 'Benchmark.*Resolve' -benchmem ./kernel/vfs | tee $(BUILD_DIR)/reports/vfs-resolve.txt

lint:
	$(BUILD_ENV) go vet ./...
	@test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }
	@tests/lint/unsafe_boundary.sh --self-test >/dev/null
	@tests/lint/unsafe_boundary.sh
	@tests/lint/no_kernel_lock.sh --self-test >/dev/null
	@tests/lint/no_kernel_lock.sh

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './build/*')

docs:
	@test -f README.md
	@test -f docs/architecture/overview.md
	@test -f docs/architecture/processes.md
	@test -f docs/kernel/native-images.md
	@test -f docs/architecture/capabilities.md
	@test -f docs/architecture/ipc.md
	@test -f docs/userspace/tty.md
	@test -f docs/userspace/ramfs.md
	@test -f docs/userspace/vfs.md
	@test -f docs/userspace/init.md
	@test -f docs/userspace/shell.md
	@test -f docs/userspace/configuration.md
	@test -f docs/userspace/storage.md
	@test -f docs/kernel/streams.md
	@test -f docs/development/testing.md
	@test -f docs/adr/0072-per-process-resource-quotas.md
	@test -f docs/adr/0073-persistent-log-service.md
	@test -f docs/adr/0074-typed-configuration-service.md
	@test -f docs/adr/0075-interrupt-driven-console.md
	@test -f docs/adr/0076-components-consult-configuration.md
	@test -f docs/adr/0077-refusing-a-volume-without-losing-the-system.md
	@test -f docs/adr/0078-restarting-the-storage-driver.md
	@test -f docs/adr/0079-command-help-and-diagnostics.md
	@test -f docs/adr/0080-commands-as-separate-images.md
	@test -f docs/adr/0081-shared-read-only-image-pages.md
	@test -f docs/adr/0082-userspace-terminal-service.md
	@test -f docs/adr/0083-user-stack-guard-pages.md
	@test -f docs/adr/0084-filesystems-out-of-the-normal-kernel.md
	@test -f docs/adr/0085-in-guest-microbenchmarks.md
	@test -f docs/adr/0086-pinned-toolchain-and-reproducible-builds.md
	@test -f docs/adr/0087-bounded-boot-archive.md
	@test -f docs/adr/0088-stack-guard-gap-and-frame-bound.md
	@test -f docs/adr/0089-terminal-output-and-session-restart.md
	@test -f docs/adr/0090-speed-optimised-builds.md
	@test -f docs/adr/0091-guarded-boot-stack-and-double-fault-stack.md
	@test -f docs/adr/0092-acpi-power-off.md
	@test -f docs/releases/0.1.0.md
	@test -f docs/releases/0.1.1.md
	@test -f docs/releases/0.1.2.md
	@test -f docs/adr/0093-executable-image-launch-authority.md
	@test -f docs/adr/0094-debug-console-capability-policy.md
	@test -f docs/adr/0095-userspace-runtime-and-unsafe-boundary.md
	@test -f docs/adr/0096-separate-fat32-filesystem-service.md
	@test -f docs/adr/0097-executable-objects.md
	@test -f docs/adr/0098-file-cache-between-vfs-and-filesystem-service.md
	@test -f docs/adr/0099-restartable-vfs.md
	@test -f docs/adr/0100-config-and-log-service-restart.md
	@test -f docs/adr/0101-restartable-block-service.md
	@test -f docs/releases/0.1.3.md
	@test -f docs/adr/0102-scalable-fat32.md
	@test -f docs/adr/0103-physical-memory-beyond-the-first-gib.md
	@test -f docs/adr/0104-native-virtual-memory-regions.md
	@test -f docs/adr/0105-page-backed-ramfs.md
	@test -f docs/adr/0106-lazily-loaded-executables.md
	@test -f docs/adr/0107-bulk-io-through-shared-memory.md
	@test -f docs/adr/0108-non-blocking-executable-preparation.md
	@test -f docs/adr/0109-variable-length-startup-object.md
	@test -f docs/adr/0110-native-name-and-path-limits.md
	@test -f docs/adr/0111-bounded-dynamic-registries.md
	@test -f docs/adr/0112-fat32-interoperability.md
	@test -f docs/adr/0115-partition-layer-and-block-slices.md
	@test -f docs/adr/0124-foreground-raw-terminal-mode-and-tui-input.md
	@test -f docs/adr/0125-independent-serial-and-framebuffer-consoles.md
	@test -f docs/userspace/file-manager.md
	@test -f docs/releases/0.1.6.md
	@test -f docs/adr/0126-native-filesystem-metadata.md
	@test -f docs/adr/0127-filesystem-capacity-and-mount-enumeration.md
	@test -f docs/adr/0128-bounded-terminal-cell-tui.md
	@test -f docs/adr/0129-safe-native-file-replacement.md
	@test -f docs/adr/0130-application-launch-and-nested-foreground.md
	@test -f docs/adr/0131-bounded-native-text-editor.md
	@test -f docs/userspace/tui.md
	@test -f docs/userspace/viewer.md
	@test -f docs/userspace/editor.md
	@test -f docs/releases/0.1.7.md
	@test -f docs/development/adding-command.md
	@test -f docs/adr/0069-discoverable-mounts.md
	@test -f docs/adr/0070-read-only-system-image-mount.md
	@test -f docs/adr/0071-persistent-volume-layout.md
	@test -f docs/adr/0009-native-kernel-call-abi.md
	@test -f docs/adr/0010-process-aware-userspace-scheduling.md
	@test -f docs/adr/0011-native-image-format.md
	@test -f docs/adr/0012-versioned-typed-ipc.md
	@test -f docs/adr/0013-shared-memory-objects.md
	@test -f docs/adr/0015-restartable-ipc-waits.md
	@test -f docs/adr/0016-userspace-tty-service.md
	@test -f docs/adr/0017-bounded-ramfs.md
	@test -f docs/adr/0018-userspace-vfs.md
	@test -f docs/adr/0019-bounded-init-supervision.md
	@test -f docs/adr/0020-uefi-initramfs-and-process-exit-events.md
	@test -f docs/adr/0021-pid1-lifecycle-control-plane.md
	@test -f docs/adr/0022-initramfs-executable-spawn.md
	@test -f docs/adr/0023-freestanding-tinygo-pid1.md
	@test -f docs/adr/0024-spawn-capability-manifests.md
	@test -f docs/adr/0025-native-shell-console-session.md
	@test -f docs/adr/0026-shell-vfs-session.md
	@test -f docs/adr/0027-native-coreutils-exec.md
	@test -f docs/adr/0028-generation-safe-task-ids-and-process-wait.md
	@test -f docs/adr/0029-process-handles-and-native-wait.md
	@test -f docs/adr/0030-bounded-system-path-native-exec.md
	@test -f docs/adr/0031-versioned-native-process-startup.md
	@test -f docs/adr/0032-native-argv-environment-and-base-utilities.md
	@test -f docs/adr/0033-bounded-block-service-protocol.md
	@test -f docs/adr/0034-capability-scoped-pci-driver-bootstrap.md
	@test -f docs/adr/0035-userspace-virtio-block-data-path.md
	@test -f docs/adr/0036-bounded-fat32-vfs-mount.md
	@test -f docs/adr/0037-explicit-standard-error-handle.md
	@test -f docs/adr/0038-explicit-standard-input-handle.md
	@test -f docs/adr/0039-bounded-standard-input-consumer.md
	@test -f docs/adr/0040-capability-protected-byte-streams.md
	@test -f docs/adr/0041-native-two-stage-pipelines.md
	@test -f docs/adr/0042-bounded-sequential-shell-execution.md
	@test -f docs/adr/0043-bounded-multistage-pipelines.md
	@test -f docs/adr/0044-bounded-stdout-file-redirection.md
	@test -f docs/adr/0045-bounded-stdout-append-redirection.md
	@test -f docs/adr/0046-bounded-stdin-file-redirection.md
	@test -f docs/adr/0052-build-time-timer-rate-and-diagnostic-build.md
	@test -f docs/adr/0053-kernel-exclusion-domain.md
	@test -f docs/adr/0054-in-guest-latency-measurement.md
	@test -f docs/adr/0055-safe-return-reschedule.md
	@test -f docs/adr/0056-sse-state-in-task-context.md
	@test -f docs/adr/0057-launch-path-and-latency-baseline.md
	@test -f docs/adr/0058-normal-and-validation-builds.md
	@test -f docs/adr/0059-structured-system-log.md
	@test -f docs/userspace/logging.md
	@test -f docs/adr/0068-timed-sleep.md
	@test -f docs/adr/0067-stopping-jobs.md
	@test -f docs/adr/0064-asynchronous-exec-and-background-jobs.md
	@test -f docs/adr/0065-kernel-heap-discipline.md
	@test -f docs/adr/0066-fat32-long-names.md
	@test -f docs/adr/0062-startup-data-through-shared-memory.md
	@test -f docs/adr/0063-mapping-shared-memory.md
	@test -f docs/adr/0061-release-shell-commands-and-system-reset.md
	@test -f docs/adr/0060-fat32-subdirectories.md
	@test -f docs/development/building.md
	@test -f docs/security/threat-model.md
	@test -f docs/security/capabilities.md
	@test -f docs/security/memory-safety.md
	@test -f docs/security/attack-surface.md
	@test -f docs/security/boot-chain.md
	@test -f docs/architecture/boot.md
	@test -f docs/architecture/filesystem.md
	@test -f docs/architecture/linux-compat.md
	@test -f docs/development/debugging.md
	@test -f docs/development/coding-style.md
	@test -f docs/development/adding-service.md
	@tests/docs/check.sh

# Rebuilds the committed static Linux fixtures from their C sources with the
# clang, lld, and musl sources TinyGo ships (userspace/linux/README.md). The
# ordinary build embeds the committed binaries and never runs this.
linux-fixtures:
	@userspace/linux/build.sh

# Linux ABI tests (docs/architecture/linux-x86_64-abi.md). The first two
# run on the host: the ELF parser against hostile and real images, and the
# pure translation code with the VFS protocol it speaks.
test-linux-elf:
	$(BUILD_ENV) go test ./kernel/compat/linux/elf ./userspace/linux ./tools/initramfs

test-linux-abi:
	$(BUILD_ENV) go test ./kernel/compat/linux ./kernel/compat/linux/abi ./kernel/vfs ./userspace/vfs

# One validation boot runs the release proofs, the hardening modes, two Linux
# processes at once, and Ctrl-C on a blocked Linux process. The musl, VFS, and
# security gates are parts of that one run, so their targets are the same.
test-linux-boot:
	@$(MAKE) --no-print-directory iso VALIDATION=1
	@$(MAKE) --no-print-directory test-disk VALIDATION=1 TEST_DISK=$(BUILD_DIR)/linux-virtio-blk.img
	@MEMORY=$(MEMORY) tests/boot/linux.sh $(ISO) $(OVMF_CODE) $(BUILD_DIR)/linux-virtio-blk.img

test-linux-musl test-linux-vfs test-linux-security: test-linux-boot

# The process-composition proofs run in the same validation boot: fork, exec,
# pipes, wait, signals, poll, and the hardening and budget modes.
test-linux-process test-linux-exec test-linux-fork test-linux-pipe test-linux-signals test-linux-poll: test-linux-boot

test-linux-all: test-linux-elf test-linux-abi test-linux-boot

# Interoperability with a static x86_64 BusyBox the caller supplies. The
# repository carries no BusyBox, so this is no part of test-all.
#   make test-linux-busybox BUSYBOX=/path/to/busybox
# LINUX_EXTRA places further Linux programs under /system/linux of the
# installed system; several paths are separated as in PATH.
LINUX_EXTRA ?=
# test-linux-shell is the same run: BusyBox sh starts programs, pipelines,
# redirections, and a script.
test-linux-shell: test-linux-busybox
test-linux-busybox:
	@test -n "$(BUSYBOX)" && test -f "$(BUSYBOX)" || { echo "BUSYBOX=/path/to/static/busybox is required" >&2; exit 2; }
	@$(MAKE) --no-print-directory iso VALIDATION=1
	@$(MAKE) --no-print-directory test-disk VALIDATION=1 LINUX_EXTRA=$(abspath $(BUSYBOX)) TEST_DISK=$(BUILD_DIR)/linux-busybox-virtio-blk.img
	@MEMORY=$(MEMORY) tests/boot/linux_busybox.sh $(ISO) $(OVMF_CODE) $(BUILD_DIR)/linux-busybox-virtio-blk.img $(notdir $(BUSYBOX))

# A static 7-Zip the caller supplies archives, tests, and extracts
# /system/linux on a diagnostic guest with SMP processors (four by default),
# and every extracted file must hash as its source. The run writes
# build/reports/linux-7zip.md. No part of test-all: the repository carries
# no 7-Zip.
#   make test-linux-7zip LINUX_EXTRA=/path/to/7zzs SMP=4
test-linux-7zip:
	@test -n "$(LINUX_EXTRA)" && test -f "$(LINUX_EXTRA)" || { echo "LINUX_EXTRA=/path/to/7zzs is required" >&2; exit 2; }
	@$(MAKE) --no-print-directory iso DIAG=1
	@$(MAKE) --no-print-directory test-disk DIAG=1 LINUX_EXTRA=$(abspath $(LINUX_EXTRA)) TEST_DISK=$(BUILD_DIR)/linux-7zip-virtio-blk.img
	@MEMORY=1G SMP=$(or $(SMP),4) tests/boot/linux_7zip.sh $(ISO) $(OVMF_CODE) $(BUILD_DIR)/linux-7zip-virtio-blk.img $(abspath $(LINUX_EXTRA))

# Profiles the kernel's locks (ADR 0187): a diagnostic guest with SMP
# processors, four by default, runs the probe's smp, kernel, and domains
# workloads, native file copies, and with LINUX_EXTRA naming a 7zzs a 7-Zip
# archive, and build/reports/lock-profile.md gives each workload's
# acquisitions and contention per lock level.
#   make profile-locks [LINUX_EXTRA=/path/to/7zzs] [SMP=4]
profile-locks:
	@$(MAKE) --no-print-directory iso DIAG=1
	@$(MAKE) --no-print-directory test-disk DIAG=1 $(if $(LINUX_EXTRA),LINUX_EXTRA=$(abspath $(LINUX_EXTRA))) TEST_DISK=$(BUILD_DIR)/lock-profile-virtio-blk.img
	@MEMORY=1G SMP=$(or $(SMP),4) tests/boot/lock_profile.sh $(ISO) $(OVMF_CODE) $(BUILD_DIR)/lock-profile-virtio-blk.img $(if $(LINUX_EXTRA),$(notdir $(LINUX_EXTRA)))

# Times four Linux calls beside their native counterparts in one guest and
# writes build/reports/linux-abi-<variant>.md. The numbers depend on the host.
test-linux-bench: iso
	@$(MAKE) --no-print-directory test-disk TEST_DISK=$(BUILD_DIR)/linux-bench-virtio-blk.img
	@MEMORY=$(MEMORY) tests/boot/linux_bench.sh $(ISO) $(OVMF_CODE) $(BUILD_DIR)/linux-bench-virtio-blk.img

# Runs the committed Linux fixtures on a Linux kernel in an amd64 container
# and requires the exit status GoOS must reproduce. It needs Docker, so it is
# a reference check and no part of test-all.
test-linux-reference:
	@userspace/linux/run-on-linux.sh | tee /dev/stderr | grep -q '^exit=0$$'

# Runs the conformance probe on a real Linux kernel booted in QEMU, where no
# user-mode emulator stands between the program and the kernel and every
# result must be the exact Linux one. The caller supplies the kernel and a
# static BusyBox for the guest's /init; the repository carries neither.
#   make test-linux-kernel-reference KERNEL=/path/to/vmlinuz BUSYBOX=/path/to/busybox
test-linux-process-reference: test-linux-kernel-reference
test-linux-kernel-reference:
	@test -n "$(KERNEL)" && test -f "$(KERNEL)" || { echo "KERNEL=/path/to/linux/kernel/image is required" >&2; exit 2; }
	@test -n "$(BUSYBOX)" && test -f "$(BUSYBOX)" || { echo "BUSYBOX=/path/to/static/busybox is required" >&2; exit 2; }
	@userspace/linux/run-on-linux-kernel.sh $(KERNEL) $(BUSYBOX)

# Proxmox hardware lab (docs/development/proxmox-lab.md). tools/pvelab boots
# the same ISO and a fresh installed disk on a remote Proxmox VE host under
# KVM. None of these targets is part of test-all or release-check: they need
# the private host and the credentials in .env.
PVE_INPUT := $(BUILD_DIR)/pve/input
PVE_ISO := $(PVE_INPUT)/goos.iso
PVE_DISK := $(PVE_INPUT)/goos-disk.img
PVELAB = $(BUILD_ENV) go run ./tools/pvelab
PVE_RUN_FLAGS = --iso $(PVE_ISO) --disk $(PVE_DISK) $(PVE_FLAGS)

pve-doctor:
	@$(PVELAB) doctor

# The lab inputs: this configuration's ISO and a new installed disk, made
# afresh on every call so no run sees what an earlier one wrote. DISK is never
# touched.
pve-artifacts: iso
	@mkdir -p $(PVE_INPUT)
	@$(MAKE) --no-print-directory test-disk TEST_DISK=$(PVE_DISK)
	@cp $(ISO) $(PVE_ISO)
	@printf 'lab inputs %s and %s\n' '$(PVE_ISO)' '$(PVE_DISK)'

test-pve-smoke: pve-artifacts
	@$(PVELAB) run --scenario smoke $(PVE_RUN_FLAGS)

test-pve-lscpu: pve-artifacts
	@$(PVELAB) run --scenario lscpu --cpu host --sockets 2 --cores 2 $(PVE_RUN_FLAGS)

test-pve-smp: pve-artifacts
	@$(PVELAB) run --scenario lscpu --cpu host --sockets 2 --cores 2 $(PVE_RUN_FLAGS)
	@$(PVELAB) run --scenario smp --cpu host --sockets 1 --cores 4 $(PVE_RUN_FLAGS)

test-pve-linux: pve-artifacts
	@$(PVELAB) run --scenario linux $(PVE_RUN_FLAGS)

# A diagnostic run of a static 7-Zip the caller supplies on four host cores.
# DIAG=1 gives the guest the diag command, whose DIAG linux line names the
# refused calls and whose DIAG sched line the parallel peaks.
#   make test-pve-7zip LINUX_EXTRA=/path/to/7zzs
test-pve-7zip:
	@test -n "$(LINUX_EXTRA)" && test -f "$(LINUX_EXTRA)" || { echo "LINUX_EXTRA=/path/to/7zzs is required" >&2; exit 2; }
	@$(MAKE) --no-print-directory pve-artifacts DIAG=1 LINUX_EXTRA=$(abspath $(LINUX_EXTRA))
	@$(PVELAB) run --scenario 7zip --cpu host --sockets 1 --cores 4 $(PVE_RUN_FLAGS)

# Boots the smoke scenario and keeps the VM for debugging; pvelab prints the
# run ID that pvelab destroy --run takes.
pve-run: pve-artifacts
	@$(PVELAB) run --scenario smoke --keep $(PVE_RUN_FLAGS)

test-pvelab:
	$(BUILD_ENV) go test ./tools/pvelab

test-hardware: test-pve-smoke test-pve-lscpu test-pve-linux

clean:
	@rm -rf $(BUILD_DIR)

distclean: clean

toolchain-check:
	@OVMF_CODE='$(OVMF_CODE)' tools/toolchain-check.sh

version:
	@printf '%s\n' '$(VERSION)'

help:
	@printf '%s\n' \
	  'GoOS build targets:' \
	  '  all/iso          build the bootable x86_64 ISO' \
	  '  kernel          build the UEFI kernel' \
	  '  run             boot with QEMU and serial on this terminal, from DISK' \
	  '  RECOVERY=1      run, run-gui: boot into recovery mode' \
	  '  system-image    build the ISO and an installed system volume image' \
	  '  disk            install the system on a new DISK (default build/goos-disk.img); refuses an existing one' \
	  '  disk-overwrite  install the system on DISK, replacing what is there' \
	  '  test-system-boot boot an installed disk and the recovery cases in QEMU' \
	  '  run-gui         boot with QEMU graphical display and serial on this terminal' \
	  '  QEMU_DISPLAY=x  GUI backend for run-gui (default cocoa; gtk/sdl on other hosts)' \
	  '  run-debug       boot paused with GDB server on tcp/1234' \
	  '  test-console-gui validate framebuffer pixels and PS/2 input headlessly' \
	  '  HZ=100|250|1000 timer rate for any build target (default 100)' \
	  '  DIAG=1          diagnostic build: kernel trace and diag dump' \
	  '  OPT=2|1|s|z     TinyGo optimisation level (default 2, fastest; z smallest)' \
	  '  MEMORY=512M     guest RAM for run and the QEMU tests, e.g. MEMORY=4G' \
	  '  version         print the release from internal/version/version.go' \
	  '  linux-fixtures  rebuild the static Linux test fixtures under userspace/linux' \
	  '  test-linux-reference run those fixtures on a Linux kernel in Docker' \
	  '  test-linux-kernel-reference KERNEL=path BUSYBOX=path  run the probe on a Linux kernel in QEMU' \
	  '  test-linux-all  Linux ABI host tests and the Linux end-to-end boot' \
	  '  test-linux-busybox BUSYBOX=path  run BusyBox applets from a static busybox' \
	  '  test-linux-7zip LINUX_EXTRA=path  archive, extract, and hash with a static 7zzs on SMP=4' \
	  '  profile-locks [LINUX_EXTRA=path]  lock acquisitions and contention per workload on SMP=4' \
	  '  test-race       concurrency tests of handles, channels, Linux tables, waits, and the scheduler under the race detector' \
	  '  test-linux-bench time Linux calls beside native ones into build/reports' \
	  '  test            run host unit and artifact integration tests' \
	  '  release-check   run the release gate: reproducible build, all tests, docs, release checks' \
	  '  test-all        run the complete validation gate, including QEMU' \
	  '  test-security   run capability, handle, and VM security tests' \
	  '  test-boot       boot QEMU headlessly and check serial markers' \
	  '  test-smp        boot four processors and prove they run tasks at once' \
	  '  test-stress     run parallel QEMU command stress (STRESS_RUNS, STRESS_ROUNDS)' \
	  '  test-storage-stress run the FAT32 model, failure, and fsck workload at scale' \
	  '  test-fat32         run the FAT32 format tests, host FAT tools included' \
	  '  test-fat32-stress  run them at scale: huge files and clusters, fuzzing' \
	  '  test-partitions    run the MBR/GPT partition and block-slice tests' \
	  '  test-latency    measure command latency in QEMU into build/reports (LATENCY_REPEATS)' \
	  '  test-microbench measure context switch, IPC, and VFS reads in QEMU (MICROBENCH_REPEATS)' \
	  '  test-performance run host and in-guest microbenchmarks into build/reports' \
	  '  test-reproducible build the ISO twice from clean and compare every artifact' \
	  '  lint/fmt/docs   quality checks' \
	  '  clean           remove build artifacts' \
	  '  toolchain-check validate tools and pinned versions (tools/toolchain.versions)' \
	  '  pve-doctor      check the Proxmox lab settings in .env (docs/development/proxmox-lab.md)' \
	  '  test-pve-smoke  boot a fresh disk on Proxmox under KVM' \
	  '  test-pve-lscpu validate 2-socket, 4-vCPU topology; test-pve-smp is an alias' \
	  '  test-pve-linux  run the static Linux programs on Proxmox' \
	  '  test-pve-7zip LINUX_EXTRA=path  run a static 7zzs on Proxmox and report refused calls' \
	  '  pve-run         boot on Proxmox and keep the VM for debugging'
