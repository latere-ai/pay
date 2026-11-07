// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package pay

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
)

// EventFunc handles one verified event.
//
// Returning an error asks the processor to retry the delivery; returning nil
// acknowledges it. A handler that cannot make sense of an event should return
// nil, not an error: an event nobody can act on will not become actionable on
// the fourth redelivery.
type EventFunc func(ctx context.Context, e Event) error

// defaultMaxBody caps a webhook body at 1 MiB. A webhook endpoint is
// unauthenticated until the signature is checked, so an unbounded read is a
// memory-exhaustion surface open to the internet.
const defaultMaxBody int64 = 1 << 20

// HandlerOption configures a webhook handler.
type HandlerOption func(*webhookHandler)

// WithMaxBody overrides the 1 MiB body cap.
func WithMaxBody(n int64) HandlerOption {
	return func(h *webhookHandler) {
		if n > 0 {
			h.maxBody = n
		}
	}
}

// WithLogger sets the logger used for deliveries that are dropped rather than
// retried, and for those acknowledged without being handled. Without one,
// nothing is logged.
func WithLogger(l *slog.Logger) HandlerOption {
	return func(h *webhookHandler) { h.log = l }
}

type webhookHandler struct {
	p       Provider
	fn      EventFunc
	maxBody int64
	log     *slog.Logger
}

// WebhookHandler is the endpoint a processor posts to.
//
// It is the only HTTP in this package, because getting the status codes wrong
// is how a product either loses a purchase or double-credits one:
//
//	400 on a body that cannot be read, a signature that does not verify, or a
//	    delivery the adapter will not turn into an Event; the body says "bad
//	    signature" only for the second, and the log's "at" tells them apart
//	200 on ErrUnconfigured and on KindIgnored (acknowledged and dropped)
//	200 when the handler returns nil
//	500 when the handler returns an error, so the processor retries
//
// A nil Provider behaves as an unconfigured one: acknowledge and drop, so a
// deployment that does not sell still answers the endpoint if it is mounted.
func WebhookHandler(p Provider, fn EventFunc, opts ...HandlerOption) http.Handler {
	h := &webhookHandler{p: p, fn: fn, maxBody: defaultMaxBody}
	for _, o := range opts {
		o(h)
	}
	return h
}

func (h *webhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.p == nil {
		h.acknowledge("no provider")
		w.WriteHeader(http.StatusOK)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, h.maxBody))
	if err != nil {
		h.drop("read body", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ev, err := h.p.ParseWebhook(body, r.Header)
	switch {
	case errors.Is(err, ErrUnconfigured):
		// Nothing is configured to act on this. Acknowledge so the processor
		// stops retrying against a deployment that will never accept it.
		h.acknowledge("provider unconfigured")
		w.WriteHeader(http.StatusOK)
		return
	case errors.Is(err, ErrBadSignature):
		h.drop("verify delivery", err)
		http.Error(w, "bad signature", http.StatusBadRequest)
		return
	case err != nil:
		// The adapter refused a delivery it may have authenticated: a body it
		// cannot decode, or one it will not credit. Calling that a bad
		// signature sends an operator hunting for the wrong cause.
		h.drop("parse delivery", err)
		http.Error(w, "unusable delivery", http.StatusBadRequest)
		return
	}
	if ev.Kind == KindIgnored {
		w.WriteHeader(http.StatusOK)
		return
	}
	if h.fn == nil {
		h.acknowledge("no handler")
		w.WriteHeader(http.StatusOK)
		return
	}
	if err := h.fn(r.Context(), ev); err != nil {
		// The only path that asks for a retry. Everything above is terminal.
		h.drop("handle event", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// drop records a delivery this handler will not act on again.
func (h *webhookHandler) drop(what string, err error) {
	if h.log == nil {
		return
	}
	h.log.Error("pay: webhook delivery dropped", "at", what, "error", err)
}

// acknowledge records a delivery answered 200 without being handled, so a
// deployment that mounted the endpoint but never wired a processor or a
// handler can see that purchases are arriving and going nowhere.
func (h *webhookHandler) acknowledge(why string) {
	if h.log == nil {
		return
	}
	h.log.Warn("pay: webhook delivery acknowledged without being handled", "at", why)
}
