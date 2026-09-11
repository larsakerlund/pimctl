BINARY  := pimctl
MODULE  := github.com/larsakerlund/pimctl
PREFIX  ?= $(HOME)/.local
# zsh reads completions from any directory on $fpath; this one is conventional
# for a per-user install and is what the README tells you to add.
ZSH_COMPLETION_DIR ?= $(PREFIX)/share/zsh/site-functions
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X $(MODULE)/internal/cli.Version=$(VERSION)

# What `make check` needs installed: the Go toolchain, golangci-lint (pinned in
# .golangci.yml), shellcheck, and expect(1) for the picker tests. `vuln` fetches
# govulncheck through `go run`, so it also needs the network.
#
# Every binary lives under cmd/: cmd/pimctl is what ships, cmd/tuiprobe is the
# pty test probe. internal/tools/doccheck stays out of it — it is a build gate
# run through `go run`, never installed.
.PHONY: all build test test-nocloudctx test-install vet lint fmt fmt-check doccheck vuln check install clean tui-test

all: build

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY) ./cmd/$(BINARY)

test:
	go vet ./...
	go test ./...

vet:
	go vet ./...

# The interactive picker needs a real terminal, so it is covered by expect(1)
# driving a probe binary built behind the `tuiprobe` tag. It lives in
# cmd/tuiprobe with every other binary; nothing from that tag is compiled into
# the shipped pimctl binary.
tui-test:
	sh scripts/test-tui-harness.sh
	go build -tags tuiprobe -o /tmp/pimctl-tuiprobe ./cmd/tuiprobe
	expect scripts/tui-filter-test.exp /tmp/pimctl-tuiprobe
	expect scripts/tui-toggle-test.exp /tmp/pimctl-tuiprobe
	expect scripts/tui-scope-test.exp /tmp/pimctl-tuiprobe
	expect scripts/tui-justification-test.exp /tmp/pimctl-tuiprobe

# The suite again, with every PATH entry that holds a cloudctx removed.
#
# cloudctx is optional: pimctl runs the whole daily loop against a plain
# `az login`, and -c/--all-contexts are the only things that need it. Three
# tests once asked PATH whether cloudctx existed and asserted on the answer, so
# they passed on a developer's machine and failed everywhere else. This target
# is that machine, and it is in `make check` so nobody has to remember.
#
# No CI job for it: GitHub's runners have no cloudctx, so the ordinary `test`
# job is already the without-cloudctx case. This target is for the machines that
# do have it, which is every machine that develops pimctl.
test-nocloudctx:
	@set -e; \
	clean=""; \
	for dir in $$(printf '%s' "$$PATH" | tr ':' ' '); do \
		[ -x "$$dir/cloudctx" ] || clean="$$clean$$dir:"; \
	done; \
	PATH="$${clean%:}"; export PATH; \
	if command -v cloudctx >/dev/null 2>&1; then \
		echo "test-nocloudctx: cloudctx is still on PATH; the target is not testing what it claims" >&2; \
		exit 1; \
	fi; \
	if ! command -v go >/dev/null 2>&1; then \
		echo "test-nocloudctx: stripping cloudctx also removed go from PATH" >&2; \
		exit 1; \
	fi; \
	echo "go test -race -count=1 ./...  (PATH without cloudctx)"; \
	go test -race -count=1 ./...

# install.sh, end to end, against the release it would actually install: into a
# temporary directory, then the binary is run and has to report that release,
# and a tampered archive has to be refused. It downloads from GitHub and needs a
# token that can read the repository, which is why it is not in `check` — a gate
# that fails on a train is not a gate. CI runs it on ubuntu and macOS with the
# workflow's own token.
test-install:
	sh scripts/test-install.sh

# golangci-lint is the single gate for the Go code: it runs the linters and,
# under `fmt`, the formatters (gofumpt, goimports, golines) configured in
# .golangci.yml. It says nothing about shell, and install.sh is the one file in
# this repository a stranger runs before reading anything, so shellcheck holds
# it and the shell tests to the same standard.
lint:
	golangci-lint run ./...
	shellcheck install.sh $(wildcard scripts/*.sh)

fmt:
	golangci-lint fmt ./...

fmt-check:
	golangci-lint fmt --diff ./...

# The documentation gate, for the half golangci-lint cannot express: every .go
# file must open with a comment saying what it owns, and every top-level
# declaration — unexported ones included, which no standard linter requires —
# must carry a doc comment that starts with its own name. Test files need only
# the header. `revive` and `godot` in .golangci.yml cover the rest.
doccheck:
	go run ./internal/tools/doccheck ./...

# The security gate. govulncheck reports only the vulnerabilities pimctl's own
# call graph can actually reach, so a finding here is a real one rather than a
# note about an unused corner of a dependency. It runs through `go run` so there
# is nothing to install and nothing to keep in step by hand, and under the
# module's own toolchain — which is what decides whether a standard-library
# advisory applies, so bump `toolchain` in go.mod to clear one.
vuln:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

# What has to be green before a change is done.
check: fmt-check lint vet doccheck vuln
	go test -race -count=1 ./...
	$(MAKE) test-nocloudctx
	$(MAKE) tui-test

install:
	@mkdir -p $(PREFIX)/bin $(ZSH_COMPLETION_DIR)
	go build -trimpath -ldflags '$(LDFLAGS)' -o $(PREFIX)/bin/$(BINARY) ./cmd/$(BINARY)
	@$(PREFIX)/bin/$(BINARY) completion zsh > $(ZSH_COMPLETION_DIR)/_$(BINARY)
	@echo "installed $(PREFIX)/bin/$(BINARY) ($(VERSION))"
	@echo "installed $(ZSH_COMPLETION_DIR)/_$(BINARY)"
	@echo "  if completion does not work, add this above compinit in ~/.zshrc:"
	@echo "    fpath=($(ZSH_COMPLETION_DIR) \$$fpath)"

clean:
	rm -rf bin
