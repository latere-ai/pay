// Package pgledger is the Postgres implementation of ledger.Store.
//
// It is a separate package because it holds the one thing the port cannot
// express: Bind, which returns write operations that run inside a transaction
// the caller already owns. A hold and the row it guards must commit together,
// and a pgx.Tx cannot cross an interface that an in-memory store also satisfies.
package pgledger

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migrations are the ledger schema, versioned and applied at startup. It
// depends on nothing else in a product's database: an entry names its holder
// and its group as opaque strings, with no foreign key, because a ledger must
// outlive the thing it is about. Deleting a project must not delete the record
// of what its work cost.
var migrations = []struct {
	version int
	sql     string
}{
	{1, `
CREATE TABLE ledger_entries (
	id         TEXT PRIMARY KEY,
	-- "<namespace>:<id>". One namespaced column rather than several nullable
	-- ones, so a balance is one predicate and a level added later does not
	-- reshape the table.
	holder     TEXT NOT NULL,
	-- Signed micro-USD. Positive adds to the holder, negative takes, so a
	-- balance is a SUM and never a case analysis over kinds.
	amount     BIGINT NOT NULL,
	kind       TEXT NOT NULL,
	reason     TEXT NOT NULL DEFAULT '',
	-- An outside system's idempotency key: a payment intent, a refund id, a
	-- rollup window key. Null on internally-moved entries.
	ref        TEXT,
	-- The unit of work an entry belongs to, on hold/release/debit, and both
	-- sides of a transfer. No foreign key: the ledger outlives what it records.
	grp        TEXT,
	actor      TEXT NOT NULL,
	-- Product dimensions this package stores and never interprets. A snapshot,
	-- not a reference, which is what lets a purged project still read correctly
	-- in an old statement.
	labels     JSONB,
	created_at TIMESTAMPTZ NOT NULL
);

-- Every balance is a fold over one holder, so this is the index that makes the
-- whole model affordable.
CREATE INDEX ledger_entries_holder ON ledger_entries (holder);

-- Idempotency. Partial, so internally-moved entries with no external reference
-- are unconstrained and the uniqueness binds only where an outside system's id
-- is the key.
CREATE UNIQUE INDEX ledger_entries_ref ON ledger_entries (ref) WHERE ref IS NOT NULL;

-- Settlement is exactly-once per unit of work: a retried or concurrent
-- terminal transition may not debit twice. An 'adjust' carries no such
-- constraint, so reconciling afterwards stays possible.
CREATE UNIQUE INDEX ledger_entries_one_debit ON ledger_entries (grp)
	WHERE kind = 'debit' AND grp IS NOT NULL;

-- Settlement finds a group's open hold; so does any watchdog.
CREATE INDEX ledger_entries_group ON ledger_entries (grp) WHERE grp IS NOT NULL;

-- Paging a statement reads one holder newest-first.
CREATE INDEX ledger_entries_holder_time ON ledger_entries (holder, created_at DESC);
`},
}

// DefaultMigrationLock is this migrator's advisory-lock id.
//
// A rolling deploy runs the old and new pods at once, so two processes migrate
// the same database concurrently: both find a version missing, both run its
// DDL, and the loser dies on "relation already exists". The lock serialises
// them.
//
// It is a constant a product can override because a product's other migrators
// have their own ids, and a collision would silently serialise unrelated
// migrators. Pick one nothing else in that database uses.
const DefaultMigrationLock int64 = 0x1ED6E4

// Migrate applies the ledger schema, using its own version table so it composes
// with a product's other migrations on one database without either owning the
// other's versions.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return MigrateWithLock(ctx, pool, DefaultMigrationLock)
}

// MigrateWithLock is Migrate with an explicit advisory-lock id.
func MigrateWithLock(ctx context.Context, pool *pgxpool.Pool, lock int64) error {
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS ledger_migrations (
			version    INT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("pgledger: create migrations table: %w", err)
	}
	for _, m := range migrations {
		if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			// The check lives inside the transaction, behind the lock: read it
			// outside and two servers starting together both see "missing".
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, lock); err != nil {
				return err
			}
			var exists bool
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM ledger_migrations WHERE version = $1)`, m.version).Scan(&exists); err != nil {
				return err
			}
			if exists {
				return nil
			}
			if _, err := tx.Exec(ctx, m.sql); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO ledger_migrations (version) VALUES ($1)`, m.version)
			return err
		}); err != nil {
			return fmt.Errorf("pgledger: apply migration %d: %w", m.version, err)
		}
	}
	return nil
}
