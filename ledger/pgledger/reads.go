// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package pgledger

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/pay/ledger"
	"latere.ai/x/pay/money"
)

// Balance is everything a holder received less everything spent, excluding
// holds: a hold has not been spent.
func (s *Store) Balance(ctx context.Context, h ledger.Holder) (money.Micro, error) {
	var v money.Micro
	err := s.pool.QueryRow(ctx, balanceSQL, string(h)).Scan(&v)
	return v, err
}

// Available is Balance less every open hold: what a holder may still commit.
func (s *Store) Available(ctx context.Context, h ledger.Holder) (money.Micro, error) {
	var v money.Micro
	err := s.pool.QueryRow(ctx, availableSQL, string(h)).Scan(&v)
	return v, err
}

// BalancesFor folds the settled balance of every given holder in one pass, so a
// table of accounts does not issue a query per row. A holder with no entry is
// omitted rather than zero-valued; a caller defaults a missing key to zero.
func (s *Store) BalancesFor(ctx context.Context, hs []ledger.Holder) (map[ledger.Holder]money.Micro, error) {
	out := make(map[ledger.Holder]money.Micro, len(hs))
	if len(hs) == 0 {
		return out, nil
	}
	keys := make([]string, len(hs))
	for i, h := range hs {
		keys[i] = string(h)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT holder, COALESCE(SUM(amount), 0) FROM ledger_entries
		WHERE holder = ANY($1) AND kind NOT IN ('hold','release')
		GROUP BY holder`, keys)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var h string
		var v money.Micro
		if err := rows.Scan(&h, &v); err != nil {
			return nil, err
		}
		out[ledger.Holder(h)] = v
	}
	return out, rows.Err()
}

// Entries lists a holder's statement, newest first.
func (s *Store) Entries(ctx context.Context, h ledger.Holder, p ledger.Page) ([]ledger.Entry, error) {
	limit := p.Limit
	if limit <= 0 {
		limit = 200
	}
	before := p.Before
	if before.IsZero() {
		// A sentinel far enough ahead that "no cursor" and "a cursor" are one
		// query rather than two.
		before = time.Now().UTC().AddDate(100, 0, 0)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, holder, amount, kind, reason, ref, grp, actor, labels, created_at
		FROM ledger_entries
		WHERE holder = $1 AND created_at < $2
		ORDER BY created_at DESC, id DESC
		LIMIT $3`, string(h), before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ledger.Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// EntryByRef finds the entry carrying ref, if any.
func (s *Store) EntryByRef(ctx context.Context, ref string) (ledger.Entry, bool, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, holder, amount, kind, reason, ref, grp, actor, labels, created_at
		FROM ledger_entries WHERE ref = $1`, ref)
	if err != nil {
		return ledger.Entry{}, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return ledger.Entry{}, false, rows.Err()
	}
	e, err := scanEntry(rows)
	if err != nil {
		return ledger.Entry{}, false, err
	}
	return e, true, rows.Err()
}

// NegativeHolders returns every holder in a namespace whose settled balance is
// below zero: the accounts that owe. Most-owed first, so an operator sees the
// worst debt at the top. An empty namespace spans all of them.
func (s *Store) NegativeHolders(ctx context.Context, namespace string) ([]ledger.HolderBalance, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT holder, SUM(amount) AS balance FROM ledger_entries
		WHERE kind NOT IN ('hold','release')
		  AND ($1 = '' OR holder LIKE $1 || ':%')
		GROUP BY holder
		HAVING SUM(amount) < 0
		ORDER BY balance ASC, holder ASC`, namespace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ledger.HolderBalance
	for rows.Next() {
		var hb ledger.HolderBalance
		var h string
		if err := rows.Scan(&h, &hb.Balance); err != nil {
			return nil, err
		}
		hb.Holder = ledger.Holder(h)
		out = append(out, hb)
	}
	return out, rows.Err()
}

// TotalOutstanding is every credit sold and not yet delivered: the sum over
// every holder in the given namespaces, or over all of them when none is given.
// It is the solvency figure an operator watches against what the processor
// account holds.
func (s *Store) TotalOutstanding(ctx context.Context, namespaces ...string) (money.Micro, error) {
	// A nil slice binds as NULL, and cardinality(NULL) is NULL, which makes the
	// whole predicate NULL and matches nothing. Normalise to an empty array so
	// "no namespaces" means "all of them" rather than "none".
	if namespaces == nil {
		namespaces = []string{}
	}
	var v money.Micro
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount), 0) FROM ledger_entries
		WHERE kind NOT IN ('hold','release')
		  AND (cardinality($1::text[]) = 0 OR split_part(holder, ':', 1) = ANY($1))`,
		namespaces).Scan(&v)
	return v, err
}

// scanEntry reads one row into an Entry, decoding the label JSON.
func scanEntry(rows pgx.Rows) (ledger.Entry, error) {
	var (
		e          ledger.Entry
		holder     string
		kind       string
		reason     string
		ref, group *string
		labels     []byte
	)
	if err := rows.Scan(&e.ID, &holder, &e.Amount, &kind, &reason, &ref, &group, &e.Actor, &labels, &e.CreatedAt); err != nil {
		return ledger.Entry{}, err
	}
	e.Holder = ledger.Holder(holder)
	e.Kind = ledger.Kind(kind)
	e.Reason = ledger.Reason(reason)
	if ref != nil {
		e.Ref = *ref
	}
	if group != nil {
		e.Group = *group
	}
	if len(labels) > 0 {
		if err := json.Unmarshal(labels, &e.Labels); err != nil {
			return ledger.Entry{}, err
		}
	}
	return e, nil
}
