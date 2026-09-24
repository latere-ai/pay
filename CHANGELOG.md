# Changelog

Every tag has a section here, and the section is the body of the GitHub
release. A tag without one is refused at the pre-push and fails the release
workflow. Write under `Unreleased` as work lands; `lateregate release vX.Y.Z`
turns that into the tag's section, commits, tags and pushes.

A section says what changed for whoever uses the release, not what was
committed: the commit log already holds that.

## Unreleased

### Fixed

- The Stripe adapter refuses a checkout in any currency other than USD with
  `stripe.ErrNotUSD`, before it calls Stripe. A session created in EUR used to
  take the customer's payment and then be refused when its webhook arrived, so
  nothing was credited. `ChargeSaved` refuses the same way. A customer abroad
  still pays in their own currency through Adaptive Pricing on a USD checkout,
  which is unchanged.
- An off-session charge that needed 3-D Secure is now credited. `ChargeSaved`
  reports such a charge as `pay.ChargePending` and says to wait for the
  webhook, but the Stripe adapter ignored `payment_intent.succeeded`, so the
  charge completed and nothing was credited. That event is now `pay.KindPaid`,
  with the `Ref` that `ChargeSaved` returned as `Charge.Ref` and the `Meta` it
  was given, so a charge credited on its synchronous success and again from
  its delivery posts once. Subscribe the webhook endpoint to
  `payment_intent.succeeded`. Only intents `ChargeSaved` created are credited:
  it marks them with the metadata `pay_origin: saved_method`, and the intent
  behind a checkout is left to its session.

### Changed

- The identity gate now also reads the frontend for the retired admin flag
  and refuses a second copy of the authorizer envelope, the question and
  the decision that `latere.ai/x/pkg` declares once (ci-gate v0.42.0).
  Nothing changes for a caller of pay.
- The guides match the code. Webhooks lists every delivery the handler refuses
  with 400, not only a bad signature. Running Stripe says that automatic tax
  needs both `stripe.Config.Tax` and `pay.TaxAutomatic`. Getting started
  carries a complete offline test of the money path, and the package
  documentation links to the guides.
