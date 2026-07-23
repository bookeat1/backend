# Partners Pay — questions to ask before finishing the adapter

`internal/infrastructure/payment/partnerspay` is a **template adapter**: it
compiles, satisfies `domain.PaymentGateway`, is registered as a known provider
code (`domain.ProviderPartnersPay`, seeded **disabled** in
`migrations/0011_partnerspay_provider.sql`), and every method returns a clear
"not implemented until the API contract is known" error
(`partnerspay.ErrContractUnknown`) instead of guessing a protocol.

As of 2026-07-22 we have no API documentation and no sandbox access to
Partners Pay (https://partnerspay.co) — only its public marketing page and
Google Play listing. This document is the list of things to get from them (or
from a signed contract / partner manager) before the adapter can do real
work. It is grouped the same way the FreedomPay sandbox checklist
(`freedompay-sandbox-checklist.md`) turned out to matter in practice: read
that file first for the shape of traps that don't show up until real traffic
runs through an adapter (a negative refund amount, a timestamp with no
timezone offset, a status read seconds before it settles).

What we already know from the marketing page (not a protocol, just context):
Partners Pay's core advertised product is **instant payouts** to contractors
(couriers, drivers, freelancers, self-employed professionals) plus electronic
document workflow (contracts signed via eGov Mobile EDS) and automated
accounting for them. HoReCa is explicitly listed among target customer
segments, alongside aggregators, taxi/delivery and marketplaces. An "Open
API" is advertised for embedding, but no endpoint, auth scheme or payload
shape is published. They are a licensed payment organisation (National Bank
of Kazakhstan licence No. 02-24-196, since May 2024).

This shapes question group 1 below: their advertised strength looks like
B2B2C **payouts to workers**, not necessarily **consumer card acquiring** (a
hosted checkout page for a guest paying a deposit). Do not assume the second
one exists just because the first one clearly does.

---

## 1. Does consumer card acquiring even exist?

- Do they offer a hosted payment page / checkout flow that lets a **guest**
  (not a contractor with a Partners Pay account) pay with a bank card, the
  same role FreedomPay's `/init_payment` redirect and TipTopPay's
  `/orders/create` hosted order fill today?
- Or is their product limited to payouts (business → contractor) and
  document/accounting workflow, with no consumer-facing payment collection at
  all? If so, this adapter's `Authorize`/`Capture`/`Void`/`Refund` shape
  (built for card acquiring) may be the wrong shape entirely, and the
  Partners Pay integration might instead belong behind a *different*,
  payout-shaped port than `domain.PaymentGateway` — worth a separate ADR if
  it turns out that way.
- What payment methods does the checkout page (if any) support — bank card
  only, Kaspi QR, Apple Pay / Google Pay, wallet balance?

## 2. Two-stage vs one-stage

- Is a **hold** (authorize without taking money) supported at all, the way
  FreedomPay's `pg_auto_clearing=0` and TipTopPay's
  `RequireConfirmation=true` both do? BookEat's whole payments model (spec
  §2, ADR-001) depends on two-stage: hold at booking, capture at seating,
  void on decline.
- If two-stage exists: what are the separate capture / void endpoints called,
  and is a **partial capture** (less than the held amount) accepted — e.g. the
  venue could only serve part of a pre-order?
- If Partners Pay is one-stage only (charge immediately, no hold), BookEat's
  usecase layer needs to know that explicitly rather than have this adapter
  silently fake a hold it cannot actually place.
- Is there a maximum hold lifetime after which an uncleared hold auto-clears
  or auto-expires (FreedomPay: 5 days, confirmed) — this bounds
  `PAYMENTS_HOLD_TTL` and the reconciliation worker's timing.

## 3. Idempotency of payment creation

- Do they accept an idempotency key on the create-payment call (header,
  body field, or derived from a merchant order id we control), so that a
  retried request after a timeout resolves to the same payment instead of a
  second one? This is non-negotiable (spec §8) — every adapter in this repo
  carries our own idempotency key end to end.
- What is the retry/replay window (TipTopPay: `X-Request-ID` replays for one
  hour)?

## 4. Refunds

- Are **partial** refunds supported against a captured payment?
- Which field carries the partial amount, and is the total amount, the
  currency, or anything else also required on the call? (FreedomPay's own
  `/g2g/refund` answer to exactly this question — `pg_amount` vs a dedicated
  refund-amount field — was only settled by an actual sandbox run, never by
  reading documentation. Do not assume either existing adapter's convention
  transfers.)
- On a **read-back** (status / get), how is an already-applied refund
  reported? Specifically: is the refunded amount ever reported with a sign
  convention that has to be un-inverted, the way FreedomPay's
  `pg_refund_amount` comes back **negative** from the merchant's point of
  view (a real defect the FreedomPay adapter hit and fixed — see
  `freedompay-sandbox-checklist.md`, "Defect found and fixed")? Ask
  explicitly; do not find out in production.
- Can a refund itself fail asynchronously (i.e. the create-refund call
  returns "pending" and a later webhook confirms or reverses it), or is it
  always synchronous?

## 5. Webhooks: shape, signature, retries

- Transport and encoding: JSON body, form-encoded (like FreedomPay's
  `result_url`), or something else? What are the field names?
- Signature scheme: which header(s) carry it, what is actually signed (the
  raw body like TipTopPay's `Content-HMAC`, or a
  script-name-plus-sorted-fields construction like FreedomPay's `pg_sig` —
  the two adapters already in this repo disagree with each other, so
  Partners Pay matching either is a guess, not a fact), and the hash
  algorithm.
- How many distinct notification types exist (payment authorized / captured
  / failed / expired, refund succeeded / failed — mirroring
  `domain.WebhookEventType`), and are they delivered to one URL with a type
  field, or to separate URLs per type (TipTopPay's pattern: `check`, `pay`,
  `fail`, `confirm`, `refund`, `cancel`, each its own route)?
- Retry behaviour: is a callback retried on a non-200 answer, how many times,
  over what window (FreedomPay: every 30 minutes for 2 hours, confirmed —
  this is *why* the same event WILL arrive more than once and the handler
  must be idempotent)?
- What acts as a stable event id we can key `payment_events`'s idempotency
  index on? If none is provided, we derive one from stable fields the way
  both existing adapters do — need the field names to do that.
- What is the exact byte-for-byte answer they expect back to acknowledge a
  callback (FreedomPay wants a signed XML envelope; TipTopPay wants
  `{"code":0}`)? Answering wrong makes them retry forever.

## 6. Sandbox / test environment

- Is there a sandbox base URL distinct from production, and how do we get
  test credentials?
- Test cards (numbers, expected outcomes — success, various decline
  reasons)? FreedomPay's sandbox required cardholder names to be at least two
  Latin words and had a merchant-level "test mode" that overrode transactions
  regardless of a `pg_testing_mode` flag we sent — ask whether Partners Pay
  has an equivalent merchant-level test/live switch, since that changes what
  `PARTNERSPAY_TESTING_MODE` in our config should actually do (or whether it
  is a no-op).

## 7. Status dictionary

- The full, literal set of status words for a payment (and, separately, for a
  refund) at every stage: created, held/authorized, captured, declined,
  expired, voided/cancelled, refunded (full and partial). We need every
  literal string, not just the "happy path" ones — an unmapped status in this
  adapter is deliberately made to fall back to "created" / "not succeeded"
  (see `mapping.go`) rather than ever guess "paid", so the cost of missing a
  status is degraded reconciliation, not a wrong charge — but we still need
  the real list to close that gap.

## 8. Currency, units, and timestamps

- Are amounts sent/received as an integer minor unit (tiyn, matching
  `domain.Money.AmountMinor`), a decimal string (`"100.00"`, like
  FreedomPay/TipTopPay), or something else (integer major units, a float)?
  Money in this codebase is always an integer minor unit — never a float —
  so whatever they use gets converted at the adapter boundary
  (`payment.FormatMinor` / `payment.ParseMinor` already exist for the decimal
  case; a different unit needs a different conversion, written once and
  tested).
- Currency: KZT only, or others? Is currency ever implicit (inferred from the
  merchant account) rather than sent per request?
- Timestamp format and **timezone** on every field that carries one. This is
  a proven trap: FreedomPay returns one field in local Almaty time with *no*
  UTC offset right next to another field that is proper RFC3339 UTC — see
  `freedompay-sandbox-checklist.md`. Ask explicitly for the timezone of every
  date/time field rather than assume UTC.

## 9. Rate limits, fees, tariffs

- Request rate limits per merchant / per endpoint.
- Acquiring fee / commission structure, and whether it is reflected back to
  us on a successful transaction (FreedomPay reports a net amount after its
  own fee, `pg_net_amount`, which turned out to differ from what the signed
  tariff assumed — worth asking for up front rather than reconciling after
  the fact).

## 10. Payment splitting and mass payouts

This is the area their marketing page presents as a genuine strength (instant
payouts to a contingent workforce, at volume — "11.2 billion tenge in
payouts, 835,000+ transactions" per their own published metrics) and it is
directly relevant to BookEat's restaurant settlement problem (paying out a
venue's share of a captured deposit), separate from consumer-facing card
acquiring:

- Can a single collected payment (or a batch of them) be **split** between
  multiple payout recipients (e.g. the restaurant's share vs BookEat's
  platform fee) in one API call, or does splitting have to be computed and
  requested by us as two separate payout instructions?
- What is the settlement cadence for a payout — instant, same-day, T+1?
- Minimum/maximum payout amount, and any KYC/onboarding requirement per
  recipient (restaurant) before they can receive a payout at all — this
  could be a real operational blocker if every restaurant needs its own
  Partners Pay-side registration before payouts start.
- Does the payout side use the same webhook/signature infrastructure as the
  (presumed) payment-collection side, or a separate one?

## 11. Fiscalisation (receipts)

- Do they issue a fiscal receipt for a card payment (Kazakhstan tax
  requirement for certain transaction types), or is that BookEat's own
  responsibility via a separate cash-register/fiscal-device integration?
  FreedomPay's documentation hints at fiscalised-merchant support
  (`pg_receipt_positions[...]`) that this codebase does not use yet; confirm
  whether Partners Pay has an equivalent and whether BookEat's restaurants
  need it.

---

## What is already built and ready to fill in

- `config.go` — `Config` / `ConfigFromEnv` / `Validate()` reading
  `PARTNERSPAY_API_URL`, `PARTNERSPAY_API_KEY`, `PARTNERSPAY_WEBHOOK_SECRET`,
  `PARTNERSPAY_TESTING_MODE`. The adapter never rises in bootstrap while
  these are unset, same as freedompay/tiptoppay.
- `gateway.go` — `Gateway` struct (cfg + shared `payment.Client` + logger +
  clock, same shape as the other two adapters) with every
  `domain.PaymentGateway` method present, doing the input validation that
  does not depend on the contract (non-empty ids, positive amounts, known
  currency) and then failing with `ErrContractUnknown`.
- `mapping.go` — `mapPaymentStatus` / `mapRefundStatus` scaffolds that
  recognise no status yet, with the "unknown never reads as paid/succeeded"
  rule already enforced and unit-tested.
- `webhook.go` — a documented scaffold plus one ready primitive,
  `verifyHMACSHA256`, a constant-time (via `hmac.Equal`) signature check —
  the piece that does *not* depend on their contract, so whoever wires the
  real scheme in cannot regress to a timing-unsafe `==` comparison.
- Tests for all of the above (config validation, every method's error path,
  the never-reads-as-paid mapping rule, the HMAC primitive).
- `domain.ProviderPartnersPay` registered in the closed provider set, and
  `migrations/0011_partnerspay_provider.sql` seeding it **disabled** in
  `payment_providers` (verified up/down against a real Postgres instance,
  2026-07-22).

## Estimated remaining work once the contract is known

Rough order of magnitude, assuming the answers above come back close to what
FreedomPay/TipTopPay already look like (a decimal-or-minor-unit REST/form API
with a shared-secret signature): **1–2 days** to fill in the real HTTP calls,
mapping tables and webhook parsing, plus however long a sandbox drill like
`freedompay-sandbox-checklist.md` takes to close every `TODO(contract)` with
an observed fact instead of a guess — that part is unpredictable by nature
(the FreedomPay one took a working sandbox session plus a few real card
attempts). If group 1's answer is "no consumer card acquiring, payouts
only", the estimate changes qualitatively: this file's `Authorize`/`Capture`/
`Void`/`Refund` shape would not apply, and the payout/mass-payment capability
(group 10) would need its own port design first — that is a design
conversation, not a coding task, and is not bounded by hours.
