// Package pay is the seam between a product and a card processor.
//
// It says what taking money requires — open a hosted payment page, charge a
// method somebody already authorised, verify and reduce a webhook — without
// naming the vendor, so the orchestration is testable offline and an adapter
// is one implementation rather than the only shape money can take.
//
// It is stdlib only. Adapters live in sibling packages (pay/stripe), the
// amount type in pay/money, and the ledger a product credits in pay/ledger.
// Nothing here knows a ledger exists: a product wires an Event to a ledger
// write.
//
// See specs/002-payment-port.md.
package pay
