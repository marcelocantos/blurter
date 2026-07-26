# blurter — see README.md. Never pass -j; set MAKEFLAGS here if needed.
VERSION ?= $(shell git describe --tags --abbrev=0 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build test vet fmt lint clean install

all: vet test build

build:
	go build -ldflags "$(LDFLAGS)" -o blurter .

test:
	go test ./... -count=1

vet:
	go vet ./...

fmt:
	gofmt -w .

# gofmt -l must be empty; CI enforces this.
lint:
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }

install: build
	install -m 0755 blurter $(HOME)/.local/bin/blurter

clean:
	rm -f blurter
