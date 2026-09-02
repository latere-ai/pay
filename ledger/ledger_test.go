// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package ledger_test

import (
	"testing"

	"latere.ai/x/pay/ledger"
	"latere.ai/x/pay/ledger/ledgertest"
)

func TestMemStoreSatisfiesTheContract(t *testing.T) {
	ledgertest.RunStoreContract(t, func(*testing.T) ledger.Store {
		return ledger.NewMemStore()
	})
}
