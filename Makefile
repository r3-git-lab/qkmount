# QKMount - lightweight cross-platform cloud drive mounter.
#
# On a normal machine with Go and FUSE/macFUSE/WinFsp installed:
#     make build
#
# On the reference Linux box (Go and FUSE headers under ~/.local, no root),
# the settings live in hack/env.sh and are picked up automatically below.

SHELL := /bin/bash

GO      ?= go
TAGS    ?= fuse3
BIN     ?= bin/qkmount
PREFIX  ?= $(HOME)/.local
PKG     := qkmount

# Source hack/env.sh for the recipe shell when it exists (dev box convenience).
ENVSH := $(wildcard hack/env.sh)
ENVPREFIX := $(if $(ENVSH),. ./hack/env.sh; ,)

.PHONY: all build test vet fmt run clean install uninstall icon check-cross

all: build

build:
	$(ENVPREFIX)$(GO) build -tags $(TAGS) -trimpath -ldflags "-s -w" -o $(BIN) ./cmd/qkmount
	@echo "built $(BIN)"

test:
	$(ENVPREFIX)$(GO) test -tags $(TAGS) ./...

vet:
	$(ENVPREFIX)$(GO) vet -tags $(TAGS) ./...

fmt:
	$(ENVPREFIX)$(GO) fmt ./...

run: build
	$(BIN) serve

clean:
	rm -rf bin

# Build the binary (if needed) then install user-level assets.
install: build
	PREFIX="$(PREFIX)" QKMOUNT_BIN="$(abspath $(BIN))" ./install.sh

uninstall:
	PREFIX="$(PREFIX)" ./install.sh --uninstall

# Cross-compiling needs a C toolchain for the target OS (FUSE/macFUSE/WinFsp
# are linked through cgo), so these are best run natively on each platform.
check-cross:
	@echo "QKMount uses cgo (FUSE/macFUSE/WinFsp); build natively on each OS."
	@echo "  Linux:   make build            (needs libfuse3-dev)"
	@echo "  macOS:   make build TAGS=      (needs macFUSE)"
	@echo "  Windows: make build TAGS=      (needs WinFsp)"
