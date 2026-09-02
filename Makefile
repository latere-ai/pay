# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: MIT

# The verification contract for pay.
#
# Every target here is one latere-ai/ci's go-verify workflow probes for and
# runs, so `make <target>` on a laptop is the same check the runner performs.
# The gates themselves live in latere.ai/x/ci-gate, pinned in go.mod; what
# each one asserts for this repository is in .lateregate.yaml.

.PHONY: check all test test-race test-hermetic cover cover-html fuzz fmt fmt-check lint lint-config lint-modernize spec-lint validate no-vendor-leak tidy hooks

all: fmt-check lint test cover spec-lint validate

# vet before test, because a vet finding is a fact about the code that does
# not need the suite to run to be true.
test:
	@go tool lateregate test

# The suite under the race detector. The ledger is written for concurrent
# holders, so this is the target that exercises the claim.
test-race:
	@go tool lateregate race

# The suite with only the Go toolchain on PATH. A test that depends on what
# happens to be installed passes locally and fails on a runner.
test-hermetic:
	@go tool lateregate hermetic

# Per package against the floor and the exemptions in .lateregate.yaml.
cover:
	@go tool lateregate cover

cover-html: cover
	go tool cover -html=coverage.out

# The seed corpus and a short campaign per target: a regression gate over the
# inputs that have already found a bug, not a fuzzing run.
fuzz:
	@for pkg in $$(go list ./...); do \
	  for f in $$(go test -list 'Fuzz.*' $$pkg 2>/dev/null | grep '^Fuzz' || true); do \
	    echo "fuzz $$pkg $$f"; go test -run '^$$' -fuzz "^$$f$$" -fuzztime 30s $$pkg || exit 1; \
	  done; \
	done

fmt:
	gofmt -w .

fmt-check:
	@go tool lateregate fmt-check

# Fails on code a standard library call or a language builtin already covers.
# Carries fixers golangci-lint's modernize linter does not, so it runs whether
# or not the linter does.
lint-modernize:
	@go tool lateregate modernize

# .golangci.yml is generated and gitignored: golangci-lint has no config
# inheritance, so the org's set is rendered from latere.ai/x/ci-gate on every
# run. Regenerating is what makes divergence impossible rather than merely
# detectable.
lint-config:
	@go tool lateregate golangci

# golangci-lint at the version lateregate pins, against the config it renders.
lint:
	@go tool lateregate lint

# specs/ records why each package has the shape it has, and specs/README.md
# carries a status per spec. A table nobody checks disagrees with the code
# within a milestone.
spec-lint:
	@go tool lateregate spec-lint

# The repo-specific check the shared pipeline cannot know about.
validate: no-vendor-leak

# A vendor SDK reachable from money/ or ledger/ would mean the port is no
# longer vendor-neutral, which is the one property specs/002-payment-port.md
# exists to hold. Nothing else here would report it: the build succeeds either
# way.
no-vendor-leak:
	@if go list -deps ./money/... ./ledger/... 2>/dev/null | grep -q 'stripe-go'; then \
	  echo "a vendor SDK leaked outside stripe/"; exit 1; \
	fi
	@echo "no vendor SDK outside the adapter"

tidy:
	go mod tidy

# hooks installs the repository git hooks (pre-commit gofmt and go fix guards).
hooks:
	git config core.hooksPath .githooks
	@[ -e CLAUDE.md ] || [ -L CLAUDE.md ] || ln -s AGENTS.md CLAUDE.md
	@echo "installed git hooks (core.hooksPath=.githooks)"

# The whole shared bar. Every gate lives in lateregate, pinned as a tool in
# go.mod; this target is a name for `go tool lateregate` and nothing else.
# The plan: `go tool lateregate list`. One gate: `go tool lateregate <gate>`.
check:
	@go tool lateregate
