.PHONY: test race cover cover-html fuzz lint lint-modernize tidy fmt fmt-check hooks

COVER_MIN := 95

test:
	go test ./...

race:
	go test -race ./...

# COVER_PKGS is every package whose statements the floor applies to: ./... minus
# the two conformance suites, paytest and ledgertest.
#
# Those are test-support. Most of their remaining statements are t.Errorf calls
# that run only when the implementation under test is broken, and reaching them
# would mean faking a *testing.T rather than testing anything real. Their logic
# is exercised on every single run -- by this repo's own stores and adapters,
# and by each consumer's -- so they are covered in the sense that matters, just
# not in the sense a statement counter measures.
COVER_PKGS = $(shell go list ./... | grep -Ev '/(paytest|ledgertest)$$' | paste -sd, -)

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

fuzz:
	@for pkg in $$(go list ./...); do \
	  for f in $$(go test -list 'Fuzz.*' $$pkg 2>/dev/null | grep '^Fuzz' || true); do \
	    echo "fuzz $$pkg $$f"; go test -run '^$$' -fuzz "^$$f$$" -fuzztime 30s $$pkg || exit 1; \
	  done; \
	done

lint: lint-modernize
	go vet ./...
	gofmt -l .

# lint-modernize fails on code that a standard library call already covers.
# It runs the toolchain modernizers, which overlap golangci-lint's modernize
# linter but add three it does not carry: buildtag, hostport, and the
# go:fix inline directives. newexpr and errorsastype are off for the reasons
# recorded in .golangci.yml.
# Only a non-empty patch fails the target. go fix also exits non-zero when a
# package does not type-check, which is a build error rather than a finding,
# so stderr is dropped and the decision rests on the patch alone.
lint-modernize:
	@for fixer in newexpr errorsastype; do \
		go tool fix help 2>&1 | grep -q "^    $$fixer " || { \
			echo "go fix no longer carries the $$fixer fixer, so -$$fixer=false is rejected and this check passes silently"; \
			exit 1; \
		}; \
	done
	@patch=$$(go fix -diff -newexpr=false -errorsastype=false ./... 2>/dev/null); \
	if [ -n "$$patch" ]; then \
		echo "$$patch"; \
		echo "go fix: the diff above is already in the standard library; apply it with go fix"; \
		exit 1; \
	fi

tidy:
	go mod tidy

# fmt formats all Go sources in place.
fmt:
	gofmt -w .

# fmt-check fails if any Go source is not gofmt-formatted.
fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt: unformatted files:"; echo "$$out"; exit 1; fi

# hooks installs the repository git hooks (pre-commit gofmt and go fix guards).
hooks:
	git config core.hooksPath .githooks
	@[ -e CLAUDE.md ] || [ -L CLAUDE.md ] || ln -s AGENTS.md CLAUDE.md
	@echo "installed git hooks (core.hooksPath=.githooks)"
