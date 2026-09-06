# blurter — see README.md. Never pass -j; set MAKEFLAGS here if needed.
VERSION ?= $(shell git describe --tags --abbrev=0 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build test vet fmt lint gate bullseye hooks clean install

all: vet test build

build:
	go build -ldflags "$(LDFLAGS)" -o blurter .

test:
	go test ./... -count=1 -race

vet:
	go vet ./...

fmt:
	gofmt -w .

# gofmt -l must be empty; CI enforces this.
lint:
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }

# The delivery gate. scripts/hooks/pre-push runs this and refuses the push
# when it is red; CI runs the same recipe on master, tags and PRs. The last
# block is the CLI contract from conventions.md.
gate: lint vet test build
	./blurter --version
	./blurter --help
	./blurter --help-agent | head -5

bullseye: gate

# Once per clone: make git run scripts/hooks instead of .git/hooks. A relative
# path resolves against the worktree the push runs from, so one setting covers
# every `git worktree`.
hooks:
	git config core.hooksPath scripts/hooks
	@echo "core.hooksPath -> scripts/hooks"

install: build
	install -m 0755 blurter $(HOME)/.local/bin/blurter

clean:
	rm -f blurter
