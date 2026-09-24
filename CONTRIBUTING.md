# Contributing

This file is for people changing pay. If you are using the library, start with
the [README](README.md) and [`docs/`](docs/README.md).

Every bug fix ships with a test that fails without it, and every change is one
small commit whose message says why.

## Getting set up

You need Go 1.27 or newer. Nothing else is required for the default suite:

```bash
make          # format check, lint, tests, coverage, spec lint, vendor check
make check    # the full shared gate: every check CI runs
```

Install the hooks once with `make hooks`. The pre-commit hook checks
formatting and standard-library modernizations on the staged files; the
pre-push hook refuses a release tag without a changelog section and lints the
packages the push changes.

## Running the checks

| Command | What it runs |
|---|---|
| `make test` | the suite |
| `make test-race` | the suite under the race detector; the ledger is written for concurrent holders, so this is the target that exercises that claim |
| `make test-hermetic` | the suite with only the Go toolchain on `PATH` |
| `make cover` | per-package coverage against the floor below |
| `make fuzz` | every fuzz target for 30 seconds each, a regression run over the inputs that have already found bugs |
| `make lint` | golangci-lint with the shared configuration, rendered on every run |
| `make spec-lint` | the spec tree and its index |
| `make validate` | the repository-specific check: no vendor SDK is reachable from `money/` or `ledger/` |
| `make check` | every gate of the shared bar; `go tool lateregate list` names them and `go tool lateregate <gate>` runs one |

Every gate is `go tool lateregate`, pinned in `go.mod`, so a gate that fails
on a runner fails the same way on a laptop. What each gate asserts for this
repository is in [`.lateregate.yaml`](.lateregate.yaml).

## The Postgres half

`ledger/pgledger` needs a database, and without one its tests **skip rather
than fail**. A run with no database therefore reports the Postgres store as
untested rather than broken, which is how a ledger can look far less proven
than it is. Run it before any change under `ledger/`:

```bash
podman run -d --rm --name pay-pg -e POSTGRES_PASSWORD=pay -e POSTGRES_USER=pay \
  -e POSTGRES_DB=pay_test -p 5432:5432 postgres:17-alpine
TEST_DATABASE_URL='postgres://pay:pay@localhost:5432/pay_test?sslmode=disable' make test-race
podman stop pay-pg
```

`docker` works in place of `podman`. CI runs the same suite against Postgres
17 in its own job on every push to `main` and every pull request.

## Coverage

The floor is 95% per package, above the shared default, because this is
money. The two conformance suites, `paytest` and `ledgertest`, are exempt:
most of their remaining statements are failure reports that run only against
a broken implementation. `pgledger` is exempt from the database-free run and
covered by the Postgres job. Across the shipped packages, with the database
suite included, coverage is about 97%, which is the figure on the README
badge.

## How the code is laid out

| Package | Role |
|---|---|
| `money` | the amount type, the currency vocabulary, the rounding rule, the purchase spread. Imports nothing from this module |
| `pay` (root) | the processor port, the webhook handler, and `MemProvider`. Standard library only |
| `stripe` | the one adapter, and the only package that imports a vendor SDK |
| `ledger` | the ledger port (`Ops`, `Store`), validation shared by every store, and `MemStore` |
| `ledger/pgledger` | the Postgres store and its migrations |
| `paytest`, `ledger/ledgertest` | the conformance suites every adapter and store must pass |

Imports go one way: `stripe` to the root to `money`, and `ledger` to `money`.
Only a product's `main` imports `stripe`. `make validate` fails if a vendor
SDK becomes reachable from `money/` or `ledger/`.

A new store or adapter is written against its conformance suite first. A
guarantee asserted only in one implementation's own tests is a coincidence;
one asserted in the suite holds for every implementation.

## Specs

[`specs/`](specs/README.md) is the design record, one spec per package: why
each shape is what it is, and what was tried and rejected. A change to a
package's contract updates its spec in the same change. `docs/` is written
for someone using the library and stays in that voice; reasoning that only a
contributor needs goes in the spec.

## Writing

Every sentence pay emits or carries is written for one reader, and the
register follows the reader:

- User, a person or a coding harness: the documentation under `docs/`, the
  error text a product shows from a typed error. Short and plain: what
  happened and what to do next, naming a command or a page, never a package,
  a function, a table, or a Kubernetes object.
- Contributor, someone changing pay: specs, this file, package
  documentation, commit messages, source comments. Precise, in the project's
  own terms, with the reason a design is what it is.
- Developer, someone debugging a running system: the typed errors' `Error()`
  text, logs, webhook processing failures. Exact and complete: object,
  operation, observed value, expected value, and the underlying error.

An error has one code, one fixed user sentence in `message`, and one
developer detail in a separate field shown only on request. The canonical
statement, worked examples, and the review checklist are in the
[registers document](https://github.com/latere-ai/pkg/blob/main/docs/writing/registers.md)
in pkg. The rule applies to new text and to reviews; existing text is fixed as
it is touched.

## Releasing

Write under `## Unreleased` in [`CHANGELOG.md`](CHANGELOG.md) as work lands,
in terms of what changes for someone using the library. A release is cut with

```bash
go tool lateregate release vX.Y.Z
```

which turns `Unreleased` into the version's section, commits, tags, and
pushes. A tag without a changelog section is refused by the pre-push hook.
While the module is `v0.x`, a breaking change bumps the minor version.
