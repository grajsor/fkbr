# NAME must match product.Name in internal/product and the cmd/ directory.
NAME    ?= fkbr
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
# Static, glibc-independent builds: run unchanged on any Linux with kernel
# 3.10 or newer, on x86-64 and on ARM64.
STATIC  := CGO_ENABLED=0 GOOS=linux
ARCHES  := amd64 arm64
HOSTARCH := $(shell go env GOARCH)

.PHONY: all build dist test test-race vet clean

all: build

# Every build produces both architectures. bin/$(NAME) is a symlink to the
# one for this machine, so it is never older than dist/.
build:
	mkdir -p dist bin
	$(foreach a,$(ARCHES),$(STATIC) GOARCH=$(a) go build -trimpath -ldflags '$(LDFLAGS)' -o dist/$(NAME)-linux-$(a) ./cmd/$(NAME) &&) true
	cd dist && sha256sum $(addprefix $(NAME)-linux-,$(ARCHES)) > SHA256SUMS
	ln -sf ../dist/$(NAME)-linux-$(HOSTARCH) bin/$(NAME)

# The deliverables: one static binary per architecture, plus checksums.
dist: build
	@file $(addprefix dist/$(NAME)-linux-,$(ARCHES))
	@cat dist/SHA256SUMS

test:
	go test ./...

# The race detector needs a 48-bit VMA kernel (not the Raspberry Pi 5 kernel).
test-race:
	go test -race ./...

vet:
	go vet ./...
	test -z "$$(gofmt -l .)"

clean:
	rm -rf bin dist
