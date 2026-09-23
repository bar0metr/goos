SHELL := /bin/sh

BUILD_DIR := build
CACHE_DIR := $(BUILD_DIR)/cache
BUILD_ENV := GOCACHE=$(abspath $(CACHE_DIR)/go) XDG_CACHE_HOME=$(abspath $(CACHE_DIR))
KERNEL_DIR = $(OUT_DIR)/kernel
ISO_ROOT = $(OUT_DIR)/iso/root
EFI_BOOT = $(ISO_ROOT)/EFI/BOOT
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
COMMANDS := $(sort $(notdir $(wildcard userspace/bin/*)))
COMMAND_DIR = $(OUT_DIR)/userspace/bin
COMMAND_ELF_DIR = $(OUT_DIR)/userspace/bin-elf
COMMAND_IMAGES = $(addprefix $(COMMAND_DIR)/,$(COMMANDS))
COMMAND_ELFS = $(addsuffix .elf,$(addprefix $(COMMAND_ELF_DIR)/,$(COMMANDS)))
COMMAND_INDEX = $(OUT_DIR)/userspace/commands
COMMAND_LIBRARY := $(shell find userspace/utility userspace/commands userspace/startup userspace/shell userspace/exec userspace/wait internal/version internal/validation internal/capability internal/logfmt userspace/log kernel/handle kernel/ipc kernel/kcall kernel/ramfs kernel/vfs kernel/clock -type f)
BLOCK_IMAGE = $(OUT_DIR)/userspace/block
BLOCK_ELF = $(OUT_DIR)/userspace/block.elf
VIRTIO_BLOCK_IMAGE = $(OUT_DIR)/userspace/virtio-blk
VIRTIO_BLOCK_ELF = $(OUT_DIR)/userspace/virtio-blk.elf
LOG_IMAGE = $(OUT_DIR)/userspace/log
LOG_ELF = $(OUT_DIR)/userspace/log.elf
CONFIG_IMAGE = $(OUT_DIR)/userspace/config
CONFIG_ELF = $(OUT_DIR)/userspace/config.elf
EFI_IMAGE = $(OUT_DIR)/iso/efi.img
ISO_BOOT_IMAGE = $(ISO_ROOT)/efi.img
VARIANT_ISO = $(OUT_DIR)/goos-x86_64.iso
ISO := $(BUILD_DIR)/goos-x86_64.iso
QEMU := qemu-system-x86_64
QEMU_SHARE ?= /opt/local/share/qemu
OVMF_CODE ?= $(QEMU_SHARE)/edk2-x86_64-code.fd
QEMU_FLAGS := -machine q35 -m 512M -drive if=pflash,format=raw,readonly=on,file=$(OVMF_CODE) -drive if=none,id=goosdisk,format=raw,file=$(BUILD_DIR)/virtio-blk.img -device virtio-blk-pci,drive=goosdisk,disable-modern=on -serial stdio -display none -no-reboot
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
# Each configuration builds into its own tree, so switching HZ, DIAG, or
# VALIDATION never mixes objects built for another configuration.
OUT_DIR := $(BUILD_DIR)/variants/hz$(HZ)$(if $(filter 1,$(DIAG)),-diag)$(if $(filter 1,$(VALIDATION)),-validation)

.DEFAULT_GOAL := all

FORCE:

.PHONY: FORCE all kernel userspace initramfs iso run run-debug test test-all test-unit test-integration test-security test-boot test-stress test-latency test-performance lint fmt docs clean distclean toolchain-check help

all: iso

kernel: $(KERNEL)


$(KERNEL_IO): kernel/arch/amd64/io.S
	@mkdir -p $(dir $@)
	clang -target x86_64-unknown-windows-gnu -ffreestanding -c $< -o $@

$(KERNEL): $(shell find kernel internal -type f) go.mod $(KERNEL_IO)
	@mkdir -p $(KERNEL_DIR)
	$(BUILD_ENV) SOURCE_DATE_EPOCH=$(SOURCE_DATE_EPOCH) tinygo build $(TINYGO_TAGS) -target=tinygo/targets/goos-amd64.json -opt=z -no-debug -o $@ ./kernel
	@nm -n $@ > $(KERNEL_DIR)/goos.sym 2>/dev/null || true

userspace: $(INIT_IMAGE) $(SERVICE_IMAGE) $(VFS_IMAGE) $(SHELL_IMAGE) $(COMMAND_IMAGES) $(COMMAND_INDEX) $(BLOCK_IMAGE) $(VIRTIO_BLOCK_IMAGE) $(LOG_IMAGE) $(CONFIG_IMAGE)

$(INIT_RUNTIME): userspace/runtime/amd64.S
	@mkdir -p $(dir $@)
	clang -target x86_64-unknown-none-elf -ffreestanding -mno-red-zone -c $< -o $@

$(INIT_ELF): $(shell find userspace/init userspace/commands userspace/exec userspace/config userspace/block userspace/startup userspace/wait userspace/cmd/init internal/capability internal/validation internal/version internal/logfmt userspace/log kernel/handle kernel/ipc kernel/kcall kernel/ramfs kernel/vfs kernel/clock -type f) tinygo/targets/goos-userspace-amd64.json userspace/runtime/amd64.ld $(INIT_RUNTIME)
	@mkdir -p $(OUT_DIR)/userspace
	$(BUILD_ENV) tinygo build $(TINYGO_TAGS) -target=tinygo/targets/goos-userspace-amd64.json -opt=z -no-debug -o $@ ./userspace/cmd/init

$(INIT_IMAGE): $(INIT_ELF) tools/elf2native/main.go kernel/loader/native.go kernel/loader/native_encode.go
	$(BUILD_ENV) go run ./tools/elf2native $(INIT_ELF) $@

$(SHELL_ELF): $(shell find userspace/cmd/shell userspace/shell userspace/commands userspace/exec userspace/startup userspace/wait userspace/config kernel/handle kernel/kcall kernel/tty kernel/ipc kernel/ramfs kernel/vfs internal/capability internal/validation internal/version internal/logfmt userspace/log kernel/clock -type f) tinygo/targets/goos-userspace-amd64.json userspace/runtime/amd64.ld $(INIT_RUNTIME)
	@mkdir -p $(OUT_DIR)/userspace
	$(BUILD_ENV) tinygo build $(TINYGO_TAGS) -target=tinygo/targets/goos-userspace-amd64.json -opt=z -no-debug -o $@ ./userspace/cmd/shell

$(SHELL_IMAGE): $(SHELL_ELF) tools/elf2native/main.go kernel/loader/native.go kernel/loader/native_encode.go
	$(BUILD_ENV) go run ./tools/elf2native $(SHELL_ELF) $@

$(VFS_ELF): $(shell find userspace/cmd/vfs userspace/vfs userspace/fat32 userspace/block userspace/startup userspace/wait kernel/handle kernel/ipc kernel/kcall kernel/ramfs kernel/vfs internal/capability internal/validation internal/version internal/logfmt userspace/log kernel/clock -type f) tinygo/targets/goos-userspace-amd64.json userspace/runtime/amd64.ld $(INIT_RUNTIME)
	@mkdir -p $(OUT_DIR)/userspace
	$(BUILD_ENV) tinygo build $(TINYGO_TAGS) -target=tinygo/targets/goos-userspace-amd64.json -opt=z -no-debug -o $@ ./userspace/cmd/vfs

$(VFS_IMAGE): $(VFS_ELF) tools/elf2native/main.go kernel/loader/native.go kernel/loader/native_encode.go
	$(BUILD_ENV) go run ./tools/elf2native $(VFS_ELF) $@

.SECONDEXPANSION:
$(COMMAND_ELFS): $(COMMAND_ELF_DIR)/%.elf: $$(wildcard userspace/bin/$$*/*.go) $(COMMAND_LIBRARY) tinygo/targets/goos-userspace-amd64.json userspace/runtime/amd64.ld $(INIT_RUNTIME)
	@mkdir -p $(COMMAND_ELF_DIR)
	$(BUILD_ENV) tinygo build $(TINYGO_TAGS) -target=tinygo/targets/goos-userspace-amd64.json -opt=z -no-debug -o $@ ./userspace/bin/$*

$(COMMAND_IMAGES): $(COMMAND_DIR)/%: $(COMMAND_ELF_DIR)/%.elf tools/elf2native/main.go kernel/loader/native.go kernel/loader/native_encode.go
	@mkdir -p $(COMMAND_DIR)
	$(BUILD_ENV) go run ./tools/elf2native $< $@

# The directory itself is a prerequisite: removing a command changes no source
# file, only the listing, and the index must drop it.
$(COMMAND_INDEX): userspace/bin $(wildcard userspace/bin/*/*.go) tools/commands/main.go userspace/commands/commands.go
	@mkdir -p $(dir $@)
	$(BUILD_ENV) go run ./tools/commands userspace/bin $@

$(BLOCK_ELF): $(shell find userspace/cmd/block userspace/block userspace/startup userspace/wait kernel/handle kernel/ipc kernel/kcall internal/capability internal/validation internal/version internal/logfmt userspace/log kernel/clock -type f) tinygo/targets/goos-userspace-amd64.json userspace/runtime/amd64.ld $(INIT_RUNTIME)
	@mkdir -p $(OUT_DIR)/userspace
	$(BUILD_ENV) tinygo build $(TINYGO_TAGS) -target=tinygo/targets/goos-userspace-amd64.json -opt=z -no-debug -o $@ ./userspace/cmd/block

$(BLOCK_IMAGE): $(BLOCK_ELF) tools/elf2native/main.go kernel/loader/native.go kernel/loader/native_encode.go
	$(BUILD_ENV) go run ./tools/elf2native $(BLOCK_ELF) $@

$(VIRTIO_BLOCK_ELF): $(shell find userspace/cmd/virtio-blk userspace/startup internal/validation internal/version internal/logfmt userspace/log kernel/handle kernel/kcall kernel/clock -type f) tinygo/targets/goos-userspace-amd64.json userspace/runtime/amd64.ld $(INIT_RUNTIME)
	@mkdir -p $(OUT_DIR)/userspace
	$(BUILD_ENV) tinygo build $(TINYGO_TAGS) -target=tinygo/targets/goos-userspace-amd64.json -opt=z -no-debug -o $@ ./userspace/cmd/virtio-blk

$(VIRTIO_BLOCK_IMAGE): $(VIRTIO_BLOCK_ELF) tools/elf2native/main.go kernel/loader/native.go kernel/loader/native_encode.go
	$(BUILD_ENV) go run ./tools/elf2native $(VIRTIO_BLOCK_ELF) $@

$(LOG_ELF): $(shell find userspace/cmd/logd userspace/config userspace/startup userspace/wait kernel/handle kernel/ipc kernel/kcall kernel/ramfs kernel/vfs internal/capability internal/validation internal/version internal/logfmt userspace/log kernel/clock -type f) tinygo/targets/goos-userspace-amd64.json userspace/runtime/amd64.ld $(INIT_RUNTIME)
	@mkdir -p $(OUT_DIR)/userspace
	$(BUILD_ENV) tinygo build $(TINYGO_TAGS) -target=tinygo/targets/goos-userspace-amd64.json -opt=z -no-debug -o $@ ./userspace/cmd/logd

$(LOG_IMAGE): $(LOG_ELF) tools/elf2native/main.go kernel/loader/native.go kernel/loader/native_encode.go
	$(BUILD_ENV) go run ./tools/elf2native $(LOG_ELF) $@

$(CONFIG_ELF): $(shell find userspace/cmd/configd userspace/config userspace/startup userspace/wait kernel/handle kernel/ipc kernel/kcall kernel/ramfs kernel/vfs internal/capability internal/validation internal/version internal/logfmt userspace/log kernel/clock -type f) tinygo/targets/goos-userspace-amd64.json userspace/runtime/amd64.ld $(INIT_RUNTIME)
	@mkdir -p $(OUT_DIR)/userspace
	$(BUILD_ENV) tinygo build $(TINYGO_TAGS) -target=tinygo/targets/goos-userspace-amd64.json -opt=z -no-debug -o $@ ./userspace/cmd/configd

$(CONFIG_IMAGE): $(CONFIG_ELF) tools/elf2native/main.go kernel/loader/native.go kernel/loader/native_encode.go
	$(BUILD_ENV) go run ./tools/elf2native $(CONFIG_ELF) $@

$(SERVICE_IMAGE): tools/service-image/main.go kernel/loader/native.go kernel/loader/native_encode.go
	@mkdir -p $(OUT_DIR)/userspace
	$(BUILD_ENV) go run ./tools/service-image $@

$(BUILD_DIR)/virtio-blk.img: tools/mkfat32/main.go
	@mkdir -p $(BUILD_DIR)
	@$(BUILD_ENV) go run ./tools/mkfat32 $@

initramfs: userspace
	@mkdir -p $(OUT_DIR)/initramfs/root/system
	@cp $(INIT_IMAGE) $(OUT_DIR)/initramfs/root/system/init
	@# A command whose directory was removed leaves its image behind; the
	@# archive takes only the commands the tree still has.
	@find $(COMMAND_DIR) -type f $(foreach command,$(COMMANDS),! -name $(command)) -delete
	$(BUILD_ENV) go run ./tools/initramfs $(INIT_IMAGE) $(SERVICE_IMAGE) $(VFS_IMAGE) $(SHELL_IMAGE) $(BLOCK_IMAGE) $(VIRTIO_BLOCK_IMAGE) $(LOG_IMAGE) $(CONFIG_IMAGE) $(COMMAND_DIR) $(COMMAND_INDEX) $(OUT_DIR)/initramfs/initramfs.tar

$(EFI_IMAGE): $(KERNEL) initramfs
	@mkdir -p $(EFI_BOOT)
	@cp $(KERNEL) $(EFI_BOOT)/BOOTX64.EFI
	@cp $(OUT_DIR)/initramfs/initramfs.tar $(ISO_ROOT)/initramfs.tar
	@rm -f $@ $@.cdr $(ISO_BOOT_IMAGE)
	@hdiutil create -quiet -fs MS-DOS -format UDTO -size 8m -volname GOOS_EFI -srcfolder $(ISO_ROOT) $@
	@mv $@.cdr $@

iso: $(ISO)

$(VARIANT_ISO): $(EFI_IMAGE)
	@rm -f $@
	@dd if=$(EFI_IMAGE) of=$(ISO_BOOT_IMAGE) bs=512 skip=1 2>/dev/null
	@dd if=/dev/zero of=$(ISO_BOOT_IMAGE) bs=1 seek=8388607 count=1 conv=notrunc 2>/dev/null
	@hdiutil makehybrid -quiet -iso -joliet -eltorito-boot $(abspath $(ISO_BOOT_IMAGE)) -no-emul-boot -o $@ $(ISO_ROOT)
	@$(BUILD_ENV) go run ./tools/iso-uefi $@
	@test -s $@

# build/goos-x86_64.iso is always the most recently requested configuration;
# build/config/build names it for the test harnesses.
$(ISO): $(VARIANT_ISO) FORCE
	@cmp -s $(VARIANT_ISO) $@ || cp $(VARIANT_ISO) $@
	@mkdir -p $(dir $(BUILD_CONFIG))
	@printf '%s\n' '$(HZ) $(DIAG) $(VALIDATION)' >$(BUILD_CONFIG)
	@printf 'built %s (%s)\n' '$@' '$(notdir $(OUT_DIR))'

run: iso $(BUILD_DIR)/virtio-blk.img
	$(QEMU) $(QEMU_FLAGS) -cdrom $(ISO)

run-debug: iso $(BUILD_DIR)/virtio-blk.img
	$(QEMU) $(QEMU_FLAGS) -s -S -cdrom $(ISO)

test: test-unit test-integration test-security

test-all:
	$(MAKE) test
	$(MAKE) lint
	$(MAKE) docs
	$(MAKE) test-boot
	$(MAKE) test-stress STRESS_RUNS=2

test-unit:
	$(BUILD_ENV) go test ./...
	$(BUILD_ENV) go test -tags goos_diag ./kernel/diag
	$(BUILD_ENV) go test -tags goos_hz1000 ./kernel/clock

test-integration: kernel
	@tests/integration/kernel_artifact.sh $(KERNEL)

test-security:
	$(BUILD_ENV) go test ./internal/capability ./kernel/device ./kernel/handle ./kernel/initramfs ./kernel/ipc ./kernel/kcall ./kernel/loader ./kernel/mm/vm ./kernel/pci ./kernel/process ./kernel/ramfs ./kernel/shmem ./kernel/stream ./kernel/tty ./kernel/vfs ./userspace/block ./userspace/exec ./userspace/init ./userspace/shell ./userspace/startup ./userspace/vfs

test-boot:
	@$(MAKE) --no-print-directory iso VALIDATION=1
	@rm -f $(BUILD_DIR)/boot-test-virtio-blk.img
	@$(BUILD_ENV) go run ./tools/mkfat32 $(BUILD_DIR)/boot-test-virtio-blk.img
	@tests/boot/qemu_boot.sh $(ISO) $(OVMF_CODE) $(BUILD_DIR)/boot-test-virtio-blk.img

STRESS_RUNS ?= 4
STRESS_JOBS ?= 4
STRESS_ROUNDS ?= 15

test-stress: iso
	@tests/boot/stress_matrix.sh $(ISO) $(OVMF_CODE) $(STRESS_RUNS) $(STRESS_JOBS) $(STRESS_ROUNDS)

LATENCY_REPEATS ?= 10

test-latency: iso
	@rm -f $(BUILD_DIR)/latency-virtio-blk.img
	@$(BUILD_ENV) go run ./tools/mkfat32 $(BUILD_DIR)/latency-virtio-blk.img
	@LATENCY_REPEATS=$(LATENCY_REPEATS) tests/boot/latency.sh $(ISO) $(OVMF_CODE) $(BUILD_DIR)/latency-virtio-blk.img

test-performance:
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
	@! grep -R --include='*.go' -n '"unsafe"' kernel internal --exclude-dir=uefi --exclude-dir=vm --exclude-dir=amd64 2>/dev/null

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './build/*')

docs:
	@test -f README.md
	@test -f docs/architecture/overview.md
	@test -f docs/architecture/processes.md
	@test -f docs/architecture/native-images.md
	@test -f docs/architecture/capabilities.md
	@test -f docs/architecture/ipc.md
	@test -f docs/architecture/tty.md
	@test -f docs/architecture/ramfs.md
	@test -f docs/architecture/vfs.md
	@test -f docs/architecture/init.md
	@test -f docs/architecture/shell.md
	@test -f docs/architecture/configuration.md
	@test -f docs/architecture/storage.md
	@test -f docs/architecture/streams.md
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
	@test -f docs/adr/0084-filesystems-out-of-the-normal-kernel.md
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
	@test -f docs/architecture/logging.md
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
	@test -f docs/architecture/boot.md
	@test -f docs/architecture/filesystem.md
	@test -f docs/architecture/linux-compat.md
	@test -f docs/development/debugging.md
	@test -f docs/development/coding-style.md
	@test -f docs/development/adding-service.md

clean:
	@rm -rf $(BUILD_DIR)

distclean: clean

toolchain-check:
	@tools/toolchain-check.sh

help:
	@printf '%s\n' \
	  'GoOS build targets:' \
	  '  all/iso          build the bootable x86_64 ISO' \
	  '  kernel          build the UEFI kernel' \
	  '  run             boot with QEMU and serial on this terminal' \
	  '  run-debug       boot paused with GDB server on tcp/1234' \
	  '  HZ=100|250|1000 timer rate for any build target (default 100)' \
	  '  DIAG=1          diagnostic build: kernel trace and diag dump' \
	  '  test            run host unit and artifact integration tests' \
	  '  test-all        run the complete validation gate, including QEMU' \
	  '  test-security   run capability, handle, and VM security tests' \
	  '  test-boot       boot QEMU headlessly and check serial markers' \
	  '  test-stress     run parallel QEMU command stress (STRESS_RUNS, STRESS_ROUNDS)' \
	  '  test-latency    measure command latency in QEMU into build/reports (LATENCY_REPEATS)' \
	  '  test-performance run microbenchmarks into build/reports' \
	  '  lint/fmt/docs   quality checks' \
	  '  clean           remove build artifacts' \
	  '  toolchain-check validate local prerequisites'
