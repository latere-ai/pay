package pay

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"sync"

	"latere.ai/x/pay/money"
)

// MemHeader is the header a MemProvider reads its signature from.
const MemHeader = "X-Pay-Signature"

// MemProvider is an in-memory Provider for offline tests.
//
// CreateCheckout records its params and hands back a canned URL; ParseWebhook
// accepts a JSON-encoded Event when the signature header matches. That is
// enough to drive a product's whole money path — checkout, credit, refund,
// auto-recharge — with no processor and no network.
//
// It is also what an unconfigured deployment can mount: set Unconfigured and
// every operation returns ErrUnconfigured.
type MemProvider struct {
	// URL is returned by every CreateCheckout; empty means a canned default.
	URL string
	// Secret is the signature ParseWebhook requires; empty accepts any.
	Secret string
	// Caps are the capabilities this fake declares. Nil means checkout and
	// refund, which is the common case; set it explicitly to test a consumer's
	// ErrUnsupported paths.
	Caps []Capability
	// Unconfigured makes every operation return ErrUnconfigured.
	Unconfigured bool
	// ChargeResult is the status ChargeSaved reports. The zero value succeeds.
	ChargeResult ChargeStatus
	// ChargeErr, when set, is returned by ChargeSaved instead of a result.
	ChargeErr error

	mu sync.Mutex
	// checkouts records every checkout created, in order.
	checkouts []CheckoutParams
	// charges records every off-session charge attempted, in order.
	charges []SavedChargeParams
	// customers maps email to the handle EnsureCustomer minted.
	customers map[string]CustomerRef
	// seq numbers minted references so they are unique and readable.
	seq int
}

var _ Provider = (*MemProvider)(nil)

// Name reports the fake's identity.
func (m *MemProvider) Name() Name { return Memory }

// Has reports a declared capability. A MemProvider with no explicit Caps
// declares checkout and refund.
func (m *MemProvider) Has(c Capability) bool {
	if m.Caps == nil {
		return c == CapCheckout || c == CapRefund
	}
	return slices.Contains(m.Caps, c)
}

// Checkouts returns a copy of every checkout created, for assertions.
func (m *MemProvider) Checkouts() []CheckoutParams {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.checkouts)
}

// Charges returns a copy of every off-session charge attempted.
func (m *MemProvider) Charges() []SavedChargeParams {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.charges)
}

// CreateCheckout records the params and returns the canned URL.
func (m *MemProvider) CreateCheckout(_ context.Context, p CheckoutParams) (Checkout, error) {
	if m.Unconfigured {
		return Checkout{}, ErrUnconfigured
	}
	if !m.Has(CapCheckout) {
		return Checkout{}, ErrUnsupported
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.checkouts = append(m.checkouts, p)
	m.seq++
	url := m.URL
	if url == "" {
		url = "https://pay.test/checkout"
	}
	return Checkout{URL: url, SessionID: m.mint("cs")}, nil
}

// EnsureCustomer mints a stable handle per email, idempotently.
func (m *MemProvider) EnsureCustomer(_ context.Context, email string, _ map[string]string) (CustomerRef, error) {
	if m.Unconfigured {
		return CustomerRef{}, ErrUnconfigured
	}
	if !m.Has(CapSavedMethod) {
		return CustomerRef{}, ErrUnsupported
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.customers == nil {
		m.customers = map[string]CustomerRef{}
	}
	if ref, ok := m.customers[email]; ok {
		return ref, nil
	}
	m.seq++
	ref := CustomerRef{Provider: Memory, ID: m.mint("cus")}
	m.customers[email] = ref
	return ref, nil
}

// ChargeSaved records the attempt and reports the configured result.
func (m *MemProvider) ChargeSaved(_ context.Context, p SavedChargeParams) (Charge, error) {
	if m.Unconfigured {
		return Charge{}, ErrUnconfigured
	}
	if !m.Has(CapSavedMethod) {
		return Charge{}, ErrUnsupported
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.charges = append(m.charges, p)
	if m.ChargeErr != nil {
		return Charge{}, m.ChargeErr
	}
	m.seq++
	status := m.ChargeResult
	if status == "" {
		status = ChargeSucceeded
	}
	return Charge{Ref: m.mint("pi"), Status: status}, nil
}

// ParseWebhook verifies the signature against Secret and decodes the payload as
// an Event, so a test synthesizes exactly the deliveries a real adapter would.
func (m *MemProvider) ParseWebhook(payload []byte, h http.Header) (Event, error) {
	if m.Unconfigured {
		return Event{}, ErrUnconfigured
	}
	if m.Secret != "" && h.Get(MemHeader) != m.Secret {
		return Event{}, ErrBadSignature
	}
	var e Event
	if err := json.Unmarshal(payload, &e); err != nil {
		return Event{}, err
	}
	e.Provider = Memory
	e.Raw = payload
	return e, nil
}

// mint builds a readable unique reference. The caller holds the lock.
func (m *MemProvider) mint(prefix string) string {
	return prefix + "_" + strconv.Itoa(m.seq)
}

// MemEvent builds the JSON payload a MemProvider's ParseWebhook accepts, so a
// test does not hand-roll the encoding.
func MemEvent(kind Kind, ref string, gross money.Micro, meta map[string]string) []byte {
	b, err := json.Marshal(Event{Kind: kind, Ref: ref, Gross: gross, Meta: meta})
	if err != nil {
		// Event is strings, integers, a string map and a byte slice, so this
		// is unreachable today. It panics rather than returning the empty
		// slice the discarded error used to leave behind: a field added later
		// that does not encode would otherwise turn every caller's payload
		// into one ParseWebhook rejects, and the rejection would name the
		// wrong cause.
		panic("pay: MemEvent: " + err.Error())
	}
	return b
}
