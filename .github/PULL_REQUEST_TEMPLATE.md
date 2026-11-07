## What this changes

<!-- What is different after this change for a program that imports pay: an amount, a balance, a webhook status code, a call to the processor. -->

## Why

<!-- The problem this solves. Link the issue if there is one. -->

## Checklist

- [ ] `make check` passes on your machine, and the Postgres half ran with
      `TEST_DATABASE_URL` set if the change touches `ledger/`.
- [ ] A bug fix includes a test that fails without the fix.
- [ ] Amounts stay `int64` micro-USD, never floats, and a write API still
      takes an unsigned magnitude and applies the sign itself.
- [ ] The import direction holds: `stripe` -> root -> `money`, `ledger`
      -> `money`, and `money` imports nothing.
- [ ] A change to a store passes `ledgertest.RunStoreContract`, and a
      change to an adapter passes `paytest.RunProviderContract`.
- [ ] The docs and `CHANGELOG.md` follow the change; an incompatible one
      says so under `Unreleased`.
