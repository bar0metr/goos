SHELL := /bin/sh

BUILD_DIR := build
CACHE_DIR := $(BUILD_DIR)/cache
BUILD_ENV := GOCACHE=$(abspath $(CACHE_DIR)/go) XDG_CACHE_HOME=$(abspath $(CACHE_DIR))
KERNEL_DIR := $(BUILD_DIR)/kernel
ISO_ROOT := $(BUILD_DIR)/iso/root
EFI_BOOT := $(ISO_ROOT)/EFI/BOOT
KERNEL := $(KERNEL_DIR)/goos.efi
KERNEL_IO := $(KERNEL_DIR)/io.o
EFI_IMAGE := $(BUILD_DIR)/iso/efi.img
ISO_BOOT_IMAGE := $(ISO_ROOT)/efi.img
ISO := $(BUILD_DIR)/goos-x86_64.iso
QEMU := qemu-system-x86_64
QEMU_SHARE ?= /opt/local/share/qemu
OVMF_CODE ?= $(QEMU_SHARE)/edk2-x86_64-code.fd
QEMU_FLAGS := -machine q35 -m 512M -drive if=pflash,format=raw,readonly=on,file=$(OVMF_CODE) -serial stdio -display none -no-reboot
SOURCE_DATE_EPOCH ?= 0

.DEFAULT_GOAL := all

.PHONY: all kernel userspace initramfs iso run run-debug test test-unit test-integration test-security test-boot test-performance lint fmt docs clean distclean toolchain-check help

all: iso

kernel: $(KERNEL)

$(KERNEL_IO): kernel/arch/amd64/io.S
	@mkdir -p $(KERNEL_DIR)
	clang -target x86_64-unknown-windows-gnu -ffreestanding -c $< -o $@

$(KERNEL): $(shell find kernel -type f) go.mod $(KERNEL_IO)
	@mkdir -p $(KERNEL_DIR)
	$(BUILD_ENV) SOURCE_DATE_EPOCH=$(SOURCE_DATE_EPOCH) tinygo build -target=tinygo/targets/goos-amd64.json -opt=z -no-debug -o $@ ./kernel
	@nm -n $@ > $(KERNEL_DIR)/goos.sym 2>/dev/null || true

userspace:
	@mkdir -p $(BUILD_DIR)/userspace/services
	@printf '%s\n' 'userspace starts at Milestone 3; no placeholder binaries emitted'

initramfs: userspace
	@mkdir -p $(BUILD_DIR)/initramfs/root
	@tar -cf $(BUILD_DIR)/initramfs/initramfs.tar -C $(BUILD_DIR)/initramfs/root .

$(EFI_IMAGE): $(KERNEL)
	@mkdir -p $(EFI_BOOT)
	@cp $(KERNEL) $(EFI_BOOT)/BOOTX64.EFI
	@rm -f $@ $@.cdr $(ISO_BOOT_IMAGE)
	@hdiutil create -quiet -fs MS-DOS -format UDTO -size 8m -volname GOOS_EFI -srcfolder $(ISO_ROOT) $@
	@mv $@.cdr $@

iso: $(ISO)

$(ISO): $(EFI_IMAGE)
	@rm -f $@
	@dd if=$(EFI_IMAGE) of=$(ISO_BOOT_IMAGE) bs=512 skip=1 2>/dev/null
	@dd if=/dev/zero of=$(ISO_BOOT_IMAGE) bs=1 seek=8388607 count=1 conv=notrunc 2>/dev/null
	@hdiutil makehybrid -quiet -iso -joliet -eltorito-boot $(abspath $(ISO_BOOT_IMAGE)) -no-emul-boot -o $@ $(ISO_ROOT)
	@$(BUILD_ENV) go run ./tools/iso-uefi $@
	@test -s $@
	@printf 'built %s\n' '$@'

run: iso
	$(QEMU) $(QEMU_FLAGS) -cdrom $(ISO)

run-debug: iso
	$(QEMU) $(QEMU_FLAGS) -s -S -cdrom $(ISO)

test: test-unit test-integration test-security

test-unit:
	$(BUILD_ENV) go test ./...

test-integration: kernel
	@tests/integration/kernel_artifact.sh $(KERNEL)

test-security:
	$(BUILD_ENV) go test ./internal/capability ./kernel/handle ./kernel/ipc ./kernel/kcall ./kernel/loader ./kernel/mm/vm ./kernel/ramfs ./kernel/shmem ./kernel/tty

test-boot: iso
	@tests/boot/qemu_boot.sh $(ISO) $(OVMF_CODE)

test-performance:
	@mkdir -p $(BUILD_DIR)/reports
	@$(BUILD_ENV) go test -run '^$$' -bench BenchmarkAllocateFree -benchmem ./kernel/mm/pmm | tee $(BUILD_DIR)/reports/page-allocation.txt
	@$(BUILD_ENV) go test -run '^$$' -bench BenchmarkChannelPingPong -benchmem ./kernel/ipc | tee $(BUILD_DIR)/reports/ipc-ping-pong.txt
	@$(BUILD_ENV) go test -run '^$$' -bench BenchmarkSharedPagePayloadAccess -benchmem ./kernel/shmem | tee $(BUILD_DIR)/reports/shared-memory.txt
	@$(BUILD_ENV) go test -run '^$$' -bench BenchmarkMetadataCreateUnlink -benchmem ./kernel/ramfs | tee $(BUILD_DIR)/reports/ramfs-metadata.txt

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
	@test -f docs/adr/0009-native-kernel-call-abi.md
	@test -f docs/adr/0010-process-aware-userspace-scheduling.md
	@test -f docs/adr/0011-native-image-format.md
	@test -f docs/adr/0012-versioned-typed-ipc.md
	@test -f docs/adr/0013-shared-memory-objects.md
	@test -f docs/adr/0015-restartable-ipc-waits.md
	@test -f docs/adr/0016-userspace-tty-service.md
	@test -f docs/adr/0017-bounded-ramfs.md
	@test -f docs/development/building.md

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
	  '  test            run host unit and artifact integration tests' \
	  '  test-security   run capability, handle, and VM security tests' \
	  '  test-boot       boot QEMU headlessly and check serial markers' \
	  '  test-performance run microbenchmarks into build/reports' \
	  '  lint/fmt/docs   quality checks' \
	  '  clean           remove build artifacts' \
	  '  toolchain-check validate local prerequisites'
