# FreedomPay: Gateway/Sync API (what we built) vs Invoice API v5 (what the owner linked)

Status: **decision needed before any Invoice v5 code is written.** No Invoice
API code exists in this repo yet — this document is the input to that
decision, not an implementation.

Sources read for this comparison:
- https://docs.freedompay.kz/overview-2056348m0 (Invoice API overview)
- https://docs.freedompay.kz/create-invoice-31949694e0 (Create Invoice
  endpoint spec)
- the existing adapter's own doc comments in
  `internal/infrastructure/payment/freedompay/config.go`, `gateway.go`,
  `signature.go` (Gateway/Sync API, checked 2026-07-21)

Both pages render parts of their content client-side / as an OpenAPI viewer;
where the two Invoice pages disagree with each other (noted below), that
disagreement is itself a `TODO(verify)` — it is not resolved here.

## 1. Two different FreedomPay products, not two ways to call the same one

| | **Gateway / Sync API** (built) | **Invoice API v5** (linked, not built) |
|---|---|---|
| Root operation | `POST /init_payment` + `/g2g/*` | `POST /v5/merchant/invoice/create` |
| What it creates | A **payment** with a two-stage hold/clear/refund lifecycle we drive ourselves via `/g2g/clearing`, `/g2g/cancel`, `/g2g/refund` | An **invoice**: a billing document with a payment page attached; FreedomPay (an "aggregator", per its own wording) owns the settlement lifecycle behind it |
| Auth / signing | `pg_sig`: MD5 of `script;sorted-fields;secret_key`, sent as a form field on every request (see `signature.go`) | `X-JWS-Signature` header: JWS with HS256, over a header block (`uri`, `auth_id`, `method`, `params`, `alg`) plus a signature over the JSON body |
| Transport | `application/x-www-form-urlencoded` request, XML response | `application/json` request and response, plus `X-Request-Id` |
| Money model | Explicit two-stage: `pg_auto_clearing=0` holds, our backend calls `/g2g/clearing` to actually take the money — this is the mechanism our whole payments spec (§2, two-stage mandatory) is built around | Single creation call; `type` ("invoicing method", e.g. `"superapp"`) suggests FreedomPay decides the capture flow internally — **TODO(verify): does Invoice v5 expose an equivalent hold-then-clear step at all, or is it capture-on-pay only?** |
| Sensitive payer data | Not modelled — the guest enters card data on FreedomPay's own hosted page (`pg_redirect_url`) | Has an explicit optional `payer` object described as "encrypted user data (AES-256-GCM with the API secret key)" — a capability our current guest-checkout flow (spec §3, no PII beyond phone/email) has no counterpart for and would need a reason to use |
| Fiscal receipts | Only foreshadowed as a TODO in our adapter (`decodeXML`'s nested-parameter note); not implemented | Has a first-class `tax` array in the create-invoice request — **if BookEat ever needs fiscal receipts through FreedomPay, Invoice v5 is the product that supports it, Gateway/Sync API was never confirmed to** |
| Callback | `pg_result_url`, form POST, signed the same way as every other message, retried every 30 min for 2h (documented and implemented in `webhook.go`) | `state_callback` field, described only as "URL for reporting the status of the payment" (POST) — **TODO(verify): payload shape, retry policy, and signature of this callback are not established from the pages read; do not assume it matches `pg_result_url`'s shape** |
| Return to merchant site | `pg_success_url` / `pg_failure_url` (two URLs, we set both to the same value today) | `back_url`, one field, explicitly "GET method only" |
| Expiry | `pg_lifetime` (seconds), scope unconfirmed (checklist item #6) | `expires_at`, explicit ISO 8601 UTC timestamp — clearer contract, but still a **TODO(verify)**: what happens to a paid-but-not-yet-settled invoice if `expires_at` passes before FreedomPay's own settlement completes |

## 2. Authentication contradiction found in the docs — flagged, not resolved

The **overview** page states every Invoice API request needs `X-JWS-Signature`
(HS256, merchant secret key) and describes its structure. The **create-invoice**
OpenAPI page, read directly, marks the operation `security: []` (i.e. no
security requirement enforced at that operation) and does not list
`X-JWS-Signature` as a documented header for `create`.

**TODO(verify) — do not guess which one is authoritative.** Two docs
disagreeing on whether a merchant-authenticating header is required is exactly
the kind of thing that must be confirmed against the sandbox with a request
that deliberately omits the header, before a single line of signing code is
written. Get this from FreedomPay support in writing if the sandbox behaviour
is itself ambiguous (e.g. it accepts an unsigned create in test mode but would
reject it in production).

## 3. Which one fits "guest pays a prepayment for a booking on a payment page"

That scenario is exactly what the **Gateway/Sync API already implements** in
this repo, end to end:

- `Authorize` → `pg_redirect_url` is the payment page we send the guest to;
- the hold is NOT money taken yet (`pg_auto_clearing=0`), matching our
  cancellation-policy requirement that a booking can be released without ever
  having charged the guest;
- `Capture` / `Void` / `Refund` map directly onto "the venue confirms the
  booking", "the booking falls through before confirmation", "the guest
  cancels after paying";
- the webhook path, its retry behaviour and its signature are already
  implemented and tested (`webhook.go`, `webhook_test.go`).

Invoice v5, on today's reading, is built for a different shape of problem: a
merchant that bills a known amount to a known payer via multiple
"invoicing methods" (`type`, e.g. `"superapp"`) and cares about fiscal
receipts and encrypted payer profile data attached to the invoice itself. It
is not obviously wrong for our use case, but nothing read so far confirms it
has a two-stage hold — and the two-stage hold is not a nice-to-have here, it
is the mechanism spec §2 depends on to avoid charging a guest for a booking
that never gets confirmed.

## 4. What we would lose by picking the wrong one

- **Picking Invoice v5 without a confirmed two-stage hold** would force one of:
  charging the guest at invoice-creation time and refunding on a declined
  booking (worse guest experience, and a refund is a slower, more visible
  operation than releasing a hold), or building our own hold logic on top of
  a product that was not designed for it.
- **Staying on Gateway/Sync API when the owner actually needs invoice-level
  features** (fiscal receipts, `payer` encrypted profile, multiple
  "invoicing methods" like `superapp`) would mean re-doing this adapter later
  instead of building it once — those capabilities have no equivalent in the
  classic API as documented.
- **Switching later, after guests already have live payment links on one
  API,** means running both adapters side by side for the transition window;
  cheap in code (the port is already `domain.PaymentGateway`, a second
  provider is "a new subpackage plus one line in bootstrap" per the package
  doc comment) but not free in ops (two acquirer relationships, two callback
  routes, two reconciliation paths).

## 5. Recommendation

Keep the current Gateway/Sync API adapter as the payment path for booking
prepayments — it already matches the two-stage requirement the whole payments
spec is built on, and it is the one about to be verified against the sandbox
(see `freedompay-sandbox-checklist.md`).

Treat Invoice API v5 as a **separate, later decision** to make only if a
concrete requirement shows up that Gateway/Sync API cannot satisfy — most
plausibly fiscal receipts (`tax` array) or a payer-data feature the business
actually asks for. Before writing any Invoice v5 code:

1. Get the `X-JWS-Signature` requirement confirmed in writing (see §2).
2. Get an explicit answer from FreedomPay on whether Invoice v5 has a
   hold/capture split equivalent to `pg_auto_clearing` + `/g2g/clearing`, or
   whether it captures on payment.
3. Only then estimate the work — reusing the existing `domain.PaymentGateway`
   port, `internal/infrastructure/payment` shared client and retry logic
   either way.

No code for Invoice v5 has been written to accompany this document, per the
task's instruction to wait for the owner's decision.
