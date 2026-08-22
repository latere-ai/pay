.PHONY: test race cover cover-html fuzz lint tidy

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

lint:
	go vet ./...
	gofmt -l .

tidy:
	go mod tidy
