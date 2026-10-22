BINARY  := ronin
VERSION ?= $(shell git describe --tags --always --dirty || echo dev)
LDFLAGS := -s -w
GOFLAGS := -trimpath

PLATFORMS := \
	linux/amd64 linux/arm64 \
	windows/amd64 windows/arm64 \
	darwin/amd64 darwin/arm64

.PHONY: all build test vet cross clean

all: build

build:
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY) .

test:
	go vet ./...

vet: test

cross:
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
		out="dist/$(BINARY)-$$os-$$arch$$ext"; \
		echo "building $$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o "$$out" . || exit 1; \
	done
	@echo "done: dist/"

clean:
	go clean
