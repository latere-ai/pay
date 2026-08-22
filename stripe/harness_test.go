package stripe

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	stripe "github.com/stripe/stripe-go/v85"
)

// The harness. stripe-go lets a caller replace the HTTP backend, so the whole
// adapter runs against an httptest.Server returning recorded payloads. That is
// what makes a vendor adapter coverable at all, and it is what puts the nested
// form encoding for line_items[0][price_data][...] — the part most likely to
// break silently on an SDK bump — under assertion rather than under hope.

const (
	testSecretKey     = "sk_test_harness"
	testWebhookSecret = "whsec_harness"
)

// call is one request the SDK made, kept whole so a test can assert on the
// exact wire form rather than on the Go params that produced it.
type call struct {
	method string
	path   string
	// form merges the query string and the urlencoded body, which is where
	// stripe-go puts parameters for GET and POST respectively.
	form url.Values
	// idempotencyKey is the Idempotency-Key header, the thing that makes a
	// daemon's retry safe.
	idempotencyKey string
	authorization  string
}

// stub is an httptest server pretending to be the Stripe REST API.
type stub struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	handlers map[string]http.HandlerFunc
	calls    []call
}

func newStub(t *testing.T) *stub {
	t.Helper()
	s := &stub{t: t, handlers: map[string]http.HandlerFunc{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stub) serve(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.t.Errorf("stub: parse form for %s %s: %v", r.Method, r.URL.Path, err)
	}
	s.mu.Lock()
	s.calls = append(s.calls, call{
		method:         r.Method,
		path:           r.URL.Path,
		form:           r.Form,
		idempotencyKey: r.Header.Get("Idempotency-Key"),
		authorization:  r.Header.Get("Authorization"),
	})
	h := s.handlers[r.Method+" "+r.URL.Path]
	s.mu.Unlock()
	if h == nil {
		s.t.Errorf("stub: unexpected request %s %s", r.Method, r.URL.Path)
		writeStripeError(w, http.StatusInternalServerError, stripe.ErrorTypeAPI, "", "no handler registered")
		return
	}
	h(w, r)
}

// on registers the handler for one endpoint.
func (s *stub) on(method, path string, h http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[method+" "+path] = h
}

// json registers an endpoint that always answers with v.
func (s *stub) json(method, path string, v any) {
	s.on(method, path, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, v) })
}

// calledOnce returns the single call to method+path, failing if there was not
// exactly one. Exactly-once matters: a decline that is retried is the failure
// this adapter exists to prevent.
func (s *stub) calledOnce(method, path string) call {
	s.t.Helper()
	got := s.callsTo(method, path)
	if len(got) != 1 {
		s.t.Fatalf("%s %s was called %d times, want exactly 1", method, path, len(got))
	}
	return got[0]
}

func (s *stub) callsTo(method, path string) []call {
	s.mu.Lock()
	defer s.mu.Unlock()
	var got []call
	for _, c := range s.calls {
		if c.method == method && c.path == path {
			got = append(got, c)
		}
	}
	return got
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// writeStripeError answers the way Stripe does, so the SDK produces a real
// *stripe.Error and the adapter's taxonomy is exercised end to end rather than
// against a hand-built error value.
func writeStripeError(w http.ResponseWriter, status int, typ stripe.ErrorType, code stripe.ErrorCode, msg string, extra ...func(map[string]any)) {
	body := map[string]any{"type": string(typ), "message": msg}
	if code != "" {
		body["code"] = string(code)
	}
	for _, f := range extra {
		f(body)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": body})
}

// backend points stripe-go at the stub with retries off, so a stubbed 5xx is
// one request and a test asserting "never retried" means it.
func (s *stub) backend() stripe.Backend {
	return stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{
		URL:               stripe.String(s.srv.URL),
		HTTPClient:        s.srv.Client(),
		MaxNetworkRetries: stripe.Int64(0),
		EnableTelemetry:   stripe.Bool(false),
		// The SDK logs every non-2xx at error level. Several tests stub a
		// failure on purpose, so silence it and keep a passing run quiet.
		LeveledLogger: &stripe.LeveledLogger{Level: stripe.LevelNull},
	})
}

// newAdapter builds a configured adapter wired to s. cfg is applied first, so a
// test can turn Tax on without restating the secrets.
func newAdapter(t *testing.T, s *stub, mutate ...func(*Config)) *Adapter {
	t.Helper()
	c := Config{SecretKey: testSecretKey, WebhookSecret: testWebhookSecret}
	for _, m := range mutate {
		m(&c)
	}
	c.backend = s.backend()
	return New(c)
}

// withTax turns Stripe Tax on for one adapter.
func withTax(c *Config) { c.Tax = true }

//
// Webhook fixtures, signed here rather than fetched, so no test needs a
// network or a Stripe account.
//

// signature computes Stripe's v1 webhook signature by hand:
//
//	sig = HMAC-SHA256(secret, t || "." || payload)
//
// Written out rather than delegated to the SDK's ComputeSignature so a change
// in the SDK's signing cannot make a passing test agree with itself.
func signature(secret string, payload []byte, ts time.Time) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.", ts.Unix())
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

// signedHeader is the header Stripe sends alongside payload.
func signedHeader(secret string, payload []byte, ts time.Time) http.Header {
	h := http.Header{}
	h.Set(signatureHeader, fmt.Sprintf("t=%d,v1=%s", ts.Unix(), signature(secret, payload, ts)))
	return h
}

// signedNow is the common case: a delivery that arrived just now, signed with
// the harness secret.
func signedNow(payload []byte) http.Header {
	return signedHeader(testWebhookSecret, payload, time.Now())
}

// eventPayload builds a Stripe event envelope around obj.
//
// api_version is deliberately an old one: every delivery in this suite is
// therefore on a different API version than the pinned SDK, so the whole suite
// is a standing test that IgnoreAPIVersionMismatch is on.
func eventPayload(t testing.TB, typ string, obj any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"id":          "evt_test_1",
		"object":      "event",
		"api_version": "2019-05-16",
		"type":        typ,
		"data":        map[string]any{"object": obj},
	})
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	return b
}

// paidSession is a card purchase: completed and already paid.
func paidSession() map[string]any {
	return map[string]any{
		"id":             "cs_test_paid",
		"object":         "checkout.session",
		"amount_total":   2000,
		"currency":       "usd",
		"payment_status": "paid",
		"payment_intent": "pi_test_1",
		"customer":       "cus_test_1",
		"metadata":       map[string]string{"email": "buyer@example.test", "credited_micro": "18700000"},
		"total_details":  map[string]any{"amount_tax": 300},
	}
}
