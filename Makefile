# The verification contract for pay.
#
# Every target here is one latere-ai/ci's go-verify workflow probes for and
# runs, so `make <target>` on a laptop is the same check the runner performs.
# The gates themselves live in latere.ai/x/ci-gate, pinned in go.mod; what
# each one asserts for this repository is in .lateregate.yaml.

COVER_MIN := 95

.PHONY: all test test-race test-hermetic cover cover-html fuzz fmt fmt-check lint lint-config lint-modernize spec-lint validate no-vendor-leak tidy hooks

all: fmt-check lint test cover spec-lint validate

# vet before test, because a vet finding is a fact about the code that does
# not need the suite to run to be true.
test:
	go vet ./...
	go test ./...

# The suite under the race detector. The ledger is written for concurrent
# holders, so this is the target that exercises the claim.
test-race:
	go test -race ./...

# The suite with only the Go toolchain on PATH. A test that depends on what
# happens to be installed passes locally and fails on a runner.
test-hermetic:
	@go tool lateregate hermetic

# COVER_PKGS is every package whose statements the floor applies to: ./... minus
# the two conformance suites, paytest and ledgertest, and minus pgledger.
#
# paytest and ledgertest are test-support. Most of their remaining statements
# are t.Errorf calls that run only when the implementation under test is
# broken, and reaching them would mean faking a *testing.T rather than testing
# anything real. Their logic is exercised on every single run -- by this repo's
# own stores and adapters, and by each consumer's -- so they are covered in the
# sense that matters, just not in the sense a statement counter measures.
#
# pgledger is out for a different reason: every statement in it needs a
# Postgres server, and its tests skip without TEST_DATABASE_URL. Leaving it in
# would make this floor a measure of whether a database happened to be
# reachable. The Postgres suite runs with a real server in this repository's
# own postgres job, which is the only place that can host one.
COVER_PKGS = $(shell go list ./... | grep -Ev '/(paytest|ledgertest|pgledger)$$' | paste -sd, -)

cover:
	go test -coverprofile=coverage.out -coverpkg=$(COVER_PKGS) ./...
	@go tool cover -func=coverage.out | tail -1
	@# An empty profile (only the mode: line) means there are no statements to
	@# measure yet, which is a scaffold rather than a coverage regression. Once
	@# any package carries code, the floor binds.
	@if [ $$(grep -vc '^mode:' coverage.out) -eq 0 ]; then \
	  echo "no statements yet; coverage floor not applicable"; exit 0; \
	fi; \
	pct=$$(go tool cover -func=coverage.out | tail -1 | awk '{print $$3}' | tr -d '%'); \
	awk -v p="$$pct" -v m="$(COVER_MIN)" 'BEGIN { if (p+0 < m+0) { printf "coverage %.1f%% is below the %d%% floor\n", p, m; exit 1 } }'

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

GOLANGCI_VERSION ?= v2.13.1

# The linter CI runs, against the config lint-config renders. Without this the
# only machine that ever lints this repository is a runner.
lint: lint-config
	@go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION) run ./...

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
