# FreedomPay sandbox checklist — closing the TODO(verify) items

Run this the moment the sandbox merchant credentials arrive. Every step uses
`cmd/fpay-probe` (see its `-h`), which drives the real adapter
(`internal/infrastructure/payment/freedompay`) — it never reimplements the
signature, and it always forces `pg_testing_mode=1`.

```
export FREEDOMPAY_MERCHANT_ID=<sandbox merchant id>
export FREEDOMPAY_SECRET_KEY=<sandbox secret key>
export FREEDOMPAY_API_URL=<sandbox base URL, if it differs from https://api.freedompay.kz>
go run ./cmd/fpay-probe init
```

Each numbered item below corresponds one-to-one to a `TODO(verify)` currently
in the code. Do not close a TODO comment until the raw XML has actually been
read — a plausible guess is not a finding.

---

## 1. `pg_auto_clearing` — which value selects the two-stage flow

Code: `gateway.go`, `Authorize()`, `params.Set("pg_auto_clearing", "0")`.

Steps:
1. `go run ./cmd/fpay-probe init` — this sends `pg_auto_clearing=0`.
2. Pay the returned `payment_url` with a sandbox test card.
3. `go run ./cmd/fpay-probe status <payment_id>` and read `pg_captured` in the
   raw XML.
   - `pg_captured=0` after a successful payment confirms 0 = hold (two-stage,
     as the adapter assumes). **Nothing to change.**
   - `pg_captured=1` means `0` actually means "auto-clear immediately" — the
     flag is inverted from what the PayBox lineage does. **Flip it to `1` in
     `Authorize()` and update the doc comment in `config.go`.** This is the
     single most important check: getting it wrong charges guests before we
     intend to.
4. Record the literal `pg_captured` value observed in the Findings Log below.

## 2. `pg_payment_status` — the full value set

Code: `mapping.go`, `mapPaymentStatus()` and the constants above it.

Steps: drive one payment through **hold → clearing → refund**, and a second
one through **hold → cancel**, calling `status` after each transition:

```
go run ./cmd/fpay-probe init
# pay it
go run ./cmd/fpay-probe status <payment_id>        # expect: hold
go run ./cmd/fpay-probe clearing <payment_id> <amount>
go run ./cmd/fpay-probe status <payment_id>        # expect: cleared
go run ./cmd/fpay-probe refund <payment_id> <amount>
go run ./cmd/fpay-probe status <payment_id>        # expect: refunded

go run ./cmd/fpay-probe init
# pay it
go run ./cmd/fpay-probe cancel <payment_id>
go run ./cmd/fpay-probe status <payment_id>        # expect: cancelled/revoked
```

For each `status` call, copy the literal `pg_payment_status` value from the
raw XML into the Findings Log. If a value does not match one of the
`paymentStatus*` constants in `mapping.go`, the adapter already logs
`"freedompay unmapped payment status"` and falls back to `created` — safe, but
add the missing constant and re-map it once confirmed.

## 3. `pg_revoke_status` / `pg_refund_status` / `pg_status_clearing` — literal values

Code: `mapping.go`, `mapOperationStatus()`; `gateway.go`, `Capture()` and
`Void()`.

Steps: same sequence as #2. After `clearing`, `cancel` and `refund`, the raw
XML printed by `fpay-probe` contains `pg_status_clearing`, `pg_revoke_status`
and `pg_refund_status` respectively — copy the literal value into the
Findings Log. Only `"success"` is currently recognised as success; anything
else falls to `RefundFailed`, which is safe but wrong if the sandbox actually
uses e.g. `"1"` or `"ok"`.

## 4. Partial refund — `pg_amount` vs `pg_refund_amount`

Code: `gateway.go`, `Refund()`, `params.Set("pg_amount", ...)`.

Steps:
1. Capture a payment for, say, `200.00`.
2. `go run ./cmd/fpay-probe refund <payment_id> 50.00` (less than the captured
   amount).
3. Read `pg_refund_amount` and `pg_amount` in the raw XML response, and check
   whether the sandbox actually refunded 50.00 or the full 200.00.
   - If it refunded the full amount regardless of what we sent in `pg_amount`,
     the field name is wrong — switch to `pg_refund_amount` in `Refund()`.
   - If it refunded exactly 50.00, the current field name is correct — record
     that and remove the TODO(verify).

## 5. `pg_currency` on `/g2g/refund` — required or not

Code: `gateway.go`, `Refund()`, `params.Set("pg_currency", ...)`.

Steps: run the refund from #4 once as-is (adapter always sends `pg_currency`
today, so this specific test needs a temporary local build with that line
commented out, or reading whether the sandbox's error text ever mentions a
missing/invalid `pg_currency` on a request that already sends it — the safer
version of this check is negative: confirm the sandbox does NOT reject the
refund for including `pg_currency` it did not ask for). Record whether the
field appears to be read at all (e.g. try sending a refund in an obviously
wrong currency string and see whether the sandbox complains about it, versus
silently ignoring it).

## 6. `pg_lifetime` — bounds the link only, or the hold too

Code: `gateway.go`, `Authorize()`, `params.Set("pg_lifetime", ...)`.

Steps:
1. `FPAY_PROBE_AMOUNT=100.00 go run ./cmd/fpay-probe init` with a short
   `pg_lifetime` — this requires a one-line temporary edit to `Authorize()` or
   a throwaway `AuthorizeRequest.HoldTTL` of e.g. 2 minutes if the probe is
   extended to accept it (not wired today: the probe sends no `HoldTTL`, i.e.
   the acquirer's own default lifetime applies).
2. Let the link expire without paying.
3. Try to pay it anyway, and separately call `status` — does the payment page
   simply become unreachable while a paid hold from a payment made just before
   expiry stays open? Or does an *authorized, uncleared* hold also get
   revoked at `pg_lifetime`, independently of the "5 days auto-clear" rule
   already confirmed in the class doc comment?
4. Record the observed behaviour; if the hold is affected, `PAYMENTS_HOLD_TTL`
   (see the operational note in `gateway.go`'s `Authorize` doc comment) may
   need a second, tighter bound.

## 7. Repeated clearing — no-op error, or a second capture

Code: `gateway.go`, `Capture()`, comment above the `/g2g/clearing` call.

Steps: `go run ./cmd/fpay-probe clearing <payment_id> <amount>` twice in a row
on the same already-cleared payment. Confirm the second call answers with an
error envelope (`pg_status=error`) and NOT a second successful clearing. This
is a correctness-critical check: if the sandbox clears twice, `Capture()` must
stop treating this operation as retry-idempotent.

## 8. `pg_payment_date` timezone

Code: `mapping.go`, `parsePaymentDate()`.

Steps: note the wall-clock time (with timezone) at which you pay a sandbox
test payment, then compare it to the `pg_payment_date` in the `status` raw
XML. Confirm whether it is UTC or Almaty local time (UTC+5).

---

## Findings Log

Fill in as each item above is run. This is the source of truth for editing
the `TODO(verify)` comments afterwards — do not edit the code from memory.

| # | Item | Raw value observed | Conclusion | Code changed? |
|---|------|--------------------|------------|----------------|
| 1 | pg_auto_clearing / pg_captured | | | |
| 2 | pg_payment_status (all transitions) | | | |
| 3 | pg_revoke_status / pg_refund_status / pg_status_clearing | | | |
| 4 | pg_amount vs pg_refund_amount (partial refund) | | | |
| 5 | pg_currency required on refund | | | |
| 6 | pg_lifetime scope | | | |
| 7 | repeated clearing behaviour | | | |
| 8 | pg_payment_date timezone | | | |

When every row is filled and every corresponding `TODO(verify)` comment in
`config.go`, `gateway.go` and `mapping.go` has been either removed (confirmed
correct) or fixed (confirmed wrong), record the outcome as a durable fact in
`/home/tai/.claude/team-memory/` (a `decisions/` or `bugs/` entry, whichever
fits) so the next person does not repeat this drill from scratch.

---

# Findings Log — run of 2026-07-22 (merchant 588079, shop status "Тестовый")

Verified against the live `https://api.freedompay.kz` with `pg_testing_mode=1`.
Everything below is a **fact read off the raw XML**, not an inference.

## Confirmed

1. **Signature scheme is correct.** Both `POST /init_payment` and `POST /g2g/status_v2` were
   accepted, so `MD5(script;<fields sorted by name incl. pg_salt>;secret_key)` is right, and the
   script-name component is the last path segment (`init_payment`, `status_v2`).
2. **`init_payment` response shape:** `pg_status=ok`, `pg_payment_id` is a **numeric** id
   (e.g. `1814725568`, not a UUID), `pg_redirect_url` points at
   `https://customer.freedompay.kz/pay.html?customer=<uuid>`, plus a previously undocumented
   field **`pg_redirect_url_type`** with value `need data` — meaning the hosted page still has to
   collect the card data. The adapter ignores it; that is fine, but note it exists.
3. **`status_v2` before payment:** `pg_payment_status=new`. Also present:
   `pg_amount`, `pg_clearing_amount=0`, `pg_refund_amount=0`, empty `pg_payment_date`,
   `pg_user_email`, `pg_card_*`, and `pg_datetime` in **ISO 8601 with an explicit offset**
   (`2026-07-22T08:28:52+00:00`, i.e. UTC).
4. **`pg_captured` is NOT returned by `status_v2`** on an unpaid payment. The checklist assumed
   it would be there — the field either appears only after a successful payment or does not exist
   on this endpoint at all. Must be re-checked on a paid transaction.
5. **Merchant-level test mode.** The shop is in status «Тестовый» in the merchant cabinet, so
   *every* transaction is a test one regardless of `pg_testing_mode`; only test payment
   instruments work. Real money cannot move until the signed contract flips the shop to production.

## Still open — needs a card payment through a browser

- `pg_auto_clearing=0` semantics (hold vs immediate capture) — **the critical one**. Resolvable
  only by paying the hosted page with a sandbox test card and re-reading `status_v2`.
- The value sets of `pg_payment_status`, `pg_revoke_status`, `pg_refund_status`,
  `pg_status_clearing`.
- Partial refund field (`pg_amount` vs a dedicated one) and whether `pg_currency` is required.
- What `pg_lifetime` bounds.

Test cards are issued per-merchant: merchant cabinet → «Разработчикам», or from the FreedomPay
manager. The cardholder name must be **at least two words in Latin letters**.

## Merchant cabinet configuration observed 2026-07-22

`CHECK URL`, `RESULT URL`, `SUCCESS URL` are **empty** in the shop settings; we pass
`pg_result_url` / `pg_success_url` / `pg_failure_url` per request instead, and `init_payment`
accepted that. If FreedomPay later rejects per-request URLs, fill the cabinet fields with the
production webhook route — and remember the callback signature is computed over the **last
segment of our own result URL** (`FREEDOMPAY_RESULT_SCRIPT_NAME`).

## Findings — first real card attempt, 2026-07-22 11:30 UTC (payment 1814858248)

The owner opened the hosted page and submitted a card. Result: **declined**, but the attempt
revealed the fields `status_v2` only returns once a card has been presented:

- `pg_payment_status=error`, `pg_error_code=99999`,
  `pg_error_description=Неизвестная ошибка платежной системы` (same values duplicated in
  `pg_failure_code` / `pg_failure_description`).
- **`pg_captured=0` — the field DOES exist**, it simply is not emitted before a card is presented.
  So the two-stage check is answerable, but only on a *successful* payment.
- `pg_card_pan` masked as `4444-44XX-XXXX-6666`, `pg_card_exp`, `pg_card_brand=VI`,
  `pg_card_name` — card data comes back masked, as expected.
- `pg_reference` (RRN-like, `260722112924`) and `pg_intreference` (uuid) — internal references
  worth persisting for reconciliation and support tickets.
- **`pg_net_amount=97.10` on a `pg_amount=100`** — the gateway reports the net after its fee,
  i.e. ~2.9% on this transaction. TODO(verify): confirm on a *successful* payment and against the
  signed tariff; the settlement model in the payments spec assumed a different acquiring rate.

Open question: which sandbox test cards are valid for merchant 588079. Error 99999 is generic and
does not distinguish "wrong test card" from "issuer declined". Ask the FreedomPay manager for the
merchant's test card list.

## RESOLVED — successful payment, capture and partial refund, 2026-07-22 11:40–11:41 UTC

Payment `1814868833`, 100.00 KZT, card HUMO ****1981, `pg_auth_code=746927`.

| TODO(verify) | Answer | Evidence |
|---|---|---|
| `pg_auto_clearing=0` = two-stage hold? | **YES — the adapter was right.** Right after a successful payment: `pg_captured=0`, `pg_clearing_amount=0`. Money is held, not taken. | status_v2 at 11:41:04Z |
| `pg_status_clearing` values | `1` = capture accepted. Response also echoes `pg_clearing_amount=100`. | /g2g/clearing at 11:41:18Z |
| `pg_captured` after capture | `1`, and `pg_clearing_amount=100`. | status_v2 after the capture |
| `pg_refund_status` values | `success`, together with `pg_payment_refund_id` (a separate payment id, `1814873671`). | /g2g/refund at 11:41:35Z |
| Partial refund — which field? | `/g2g/refund` accepts **`pg_amount`** as the partial sum; no separate refund-amount parameter, and no `pg_currency` was needed. 40.00 of 100.00 went through. | /g2g/refund |
| `pg_payment_status` vocabulary | `new` → `error` (declined) → `success`. `success` alone does NOT mean captured — it must be read together with `pg_captured`. | three payments |

### Defect found and fixed

`status_v2` reports a refund as a **negative** `pg_refund_amount` (`-40`), from the merchant's
point of view. `Gateway.status()` required `refunded > 0`, so a real refund was silently ignored
and the payment stayed "captured". Fixed in `gateway.go` (take the magnitude) with a regression
case in `TestGetTranslatesTheGatewayView`.

### Other facts worth keeping

- `pg_net_amount=97.10` on a 100.00 payment — **acquiring fee ≈ 2.9%**, identical on the failed
  attempts and on the successful one, so it comes from the merchant tariff. The settlement model
  in the payments spec assumed ~1%. Must be reconciled against the signed tariff.
- `pg_auth_code` appears only on a successful payment.
- `pg_payment_date` (`2026-07-22 16:40:02`) is **local Almaty time with no offset**, while
  `pg_datetime` (`2026-07-22T11:41:04+00:00`) is UTC with an offset. Never parse `pg_payment_date`
  as UTC — it is 5 hours ahead of it.
- `status_v2` lags the capture by a few seconds: immediately after a successful `/g2g/clearing`
  it still returned `pg_captured=0`, and only the next call showed `1`. Do not treat an
  immediate read-back as proof the capture failed.
- Test-mode declines are informative: a wrong test card gives `pg_error_code=99999` "Неизвестная
  ошибка", while a card designed to fail gives "Операция отклонена по соображениям безопасности
  карточных данных".
