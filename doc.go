// Package pay is Latere's payment port: what taking money requires,
// without naming a processor. Open a hosted payment page, charge a method
// somebody already authorised, verify and reduce a webhook.
//
// It is stdlib only. Processor adapters live in sibling packages, so this
// one never imports a vendor SDK, and the ledger a product credits lives
// in latere.ai/x/pay/ledger. See specs/002-payment-port.md.
//
// The surface is not implemented yet; this file carries the package
// identity so the module builds while it is being written.
package pay
