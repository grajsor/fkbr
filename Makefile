# NAME must match product.Name in internal/product and the cmd/ directory.
NAME    ?= fkbr
# The release number lives in internal/product (Release); builds add the
# commit they were made from. Outside a git checkout (an archive, a copy
# without .git) it is "dev" and the version just v1.0; safe.directory lets
# git answer when the checkout belongs to another user (sudo make).
# "-dirty" only when git itself reports uncommitted changes.
GIT     := git -c safe.directory='*'
COMMIT  ?= $(shell $(GIT) rev-parse --short HEAD 2>/dev/null || echo dev)$(shell $(GIT) rev-parse -q --verify HEAD > /dev/null 2>&1 && ! $(GIT) diff --quiet HEAD 2>/dev/null && echo -dirty)
LDFLAGS := -s -w -X main.commit=$(COMMIT)
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
