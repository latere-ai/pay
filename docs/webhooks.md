# Webhooks

A webhook endpoint is the only place a stranger can reach your money path. Two
things decide whether it is correct: what you do with an unverified delivery,
and what status code you return.

## Mounting it

```go
mux.Handle("POST /webhooks/stripe", pay.WebhookHandler(processor, onEvent,
    pay.WithLogger(slog.Default())))
```

## The status codes are the contract

Getting these wrong is how a product loses a purchase or credits one twice.

```mermaid
flowchart TD
    IN([Delivery arrives]) --> READ{"Body readable<br/>within the cap?"}
    READ -->|no| C400["<b>400</b><br/>never retried"]
    READ -->|yes| SIG{"Signature<br/>verifies?"}
    SIG -->|no| C400
    SIG -->|unconfigured| C200A["<b>200</b><br/>acknowledged, dropped"]
    SIG -->|yes| KIND{"A kind we<br/>model?"}
    KIND -->|no| C200A
    KIND -->|yes| H["your handler"]
    H -->|nil| C200B["<b>200</b><br/>done"]
    H -->|error| C500["<b>500</b><br/>processor retries"]
```

| Situation | Code | Why |
|---|---|---|
| Signature does not verify | 400 | A processor must not retry what it cannot authenticate |
| No processor configured | 200 | Stop a processor retrying against a deployment that will never accept it |
| An event the port does not model | 200 | It will not become actionable on the fourth redelivery |
| Handler returns `nil` | 200 | Done |
| Handler returns an error | 500 | The **only** path that asks for a retry |

So: return an error from your handler **only** when a retry could succeed. A
database that is briefly down, yes. An event you cannot make sense of, no.

The body is read through a bounded reader (1 MiB, `pay.WithMaxBody` to change
it). The endpoint is unauthenticated until the signature is checked, so an
unbounded read is a memory-exhaustion surface open to the internet.

## What an event means

The adapter reduces a vendor delivery to one flat shape. Your handler never
parses vendor JSON.

| Kind | Meaning | What to do |
|---|---|---|
| `KindPaid` | Money received | `Credit`, keyed on `Ref` |
| `KindRefunded` | Refunded | `Reverse`, keyed on `ReversalRef` |
| `KindDisputed` | Charged back | `Reverse`, keyed on `ReversalRef` |
| `KindPaymentFailed` | A charge did not go through | Telemetry. Never a ledger write |
| `KindIgnored` | Not modelled | Never reaches your handler |

`Ref` is the purchase's reference and is stable across deliveries of the same
purchase. `ReversalRef` is the refund's **own** reference, distinct on purpose,
so a clawback dedupes independently of what it reverses.

## The two-delivery problem

A card pays synchronously, so `completed` is already paid. SEPA Direct Debit,
iDEAL and Bancontact — what European customers reach for — leave `completed`
**unpaid** and confirm later.

```mermaid
sequenceDiagram
    participant S as Processor
    participant A as Adapter
    participant You as Your handler

    Note over S: card
    S->>A: completed (paid)
    A->>You: KindPaid, ref pi_1

    Note over S: SEPA
    S->>A: completed (unpaid)
    A-->>S: KindIgnored — dropped
    Note over S: hours later
    S->>A: async_payment_succeeded
    A->>You: KindPaid, ref pi_1
```

Both purchases credit exactly once. The adapter emits `KindPaid` only for a
*paid* session, and the ledger's dedupe on `Ref` is the second line of defence
rather than the only one.

If you subscribe to only one of those two events you have a live bug in one
direction or the other: drop the first and card purchases never credit, drop the
second and bank transfers never do.

## Idempotency is not optional

Processors retry. Networks duplicate. Operators replay from a dashboard. Write
your handler assuming every delivery arrives more than once, and let the
ledger's `Ref` make that safe:

```go
func onEvent(ctx context.Context, e pay.Event) error {
    switch e.Kind {
    case pay.KindPaid:
        return book.Credit(ctx, ledger.Posting{…, Ref: e.Ref})
    case pay.KindRefunded, pay.KindDisputed:
        _, err := book.Reverse(ctx, ledger.Reversal{Of: e.Ref, Ref: e.ReversalRef})
        return err
    }
    return nil
}
```

**Never hand-credit a wallet to fix a webhook problem.** A manual entry has no
processor reference, so when the real delivery lands it credits again.

## Acting on a crossing

`Reverse` reports the balance either side, so your product can decide what a
crossing means without the ledger having an opinion:

```go
eff, err := book.Reverse(ctx, r)
if eff.Applied && eff.Before >= 0 && eff.After < 0 {
    freezeAccount()   // your policy, not the ledger's
}
```

`eff.Applied == false` means the reversal was already recorded.
