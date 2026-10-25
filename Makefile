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

.PHONY: all build dist release test test-race vet clean

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

# Downloads: dist/$(NAME)-linux-<arch>.tar.gz and .zip (minimal RHEL has
# no unzip), each holding the binary as $(NAME) with LICENSE and README.md. The arm64 build is plain ARMv8.0
# (GOARM64 default): it runs on a Raspberry Pi 3, 4 or 5 with a 64-bit
# OS and anything newer.
release: build
	$(foreach a,$(ARCHES),rm -rf dist/zip-$(a) dist/$(NAME)-linux-$(a).zip dist/$(NAME)-linux-$(a).tar.gz && mkdir dist/zip-$(a) && \
		cp dist/$(NAME)-linux-$(a) dist/zip-$(a)/$(NAME) && cp LICENSE README.md dist/zip-$(a)/ && \
		(cd dist/zip-$(a) && zip -q -X ../$(NAME)-linux-$(a).zip $(NAME) LICENSE README.md && \
		tar --owner=0 --group=0 -czf ../$(NAME)-linux-$(a).tar.gz $(NAME) LICENSE README.md) && rm -r dist/zip-$(a) &&) true
	cd dist && sha256sum $(foreach a,$(ARCHES),$(NAME)-linux-$(a) $(NAME)-linux-$(a).tar.gz $(NAME)-linux-$(a).zip) > SHA256SUMS

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
