# Changelog

Every tag has a section here, and the section is the body of the GitHub
release. A tag without one is refused at the pre-push and fails the release
workflow. Write under `Unreleased` as work lands; `lateregate release vX.Y.Z`
turns that into the tag's section, commits, tags and pushes.

A section says what changed for whoever uses the release, not what was
committed: the commit log already holds that.

## Unreleased

- The identity gate now also reads the frontend for the retired admin flag
  and refuses a second copy of the authorizer envelope, the question and
  the decision that `latere.ai/x/pkg` declares once (ci-gate v0.42.0).
  Nothing changes for a caller of pay.
- The guides match the code. Webhooks lists every delivery the handler refuses
  with 400, not only a bad signature. Running Stripe says a checkout created in
  a currency other than USD charges the customer and credits nothing, and that
  automatic tax needs both `stripe.Config.Tax` and `pay.TaxAutomatic`. Getting
  started carries a complete offline test of the money path, and the package
  documentation links to the guides.
