package payouts

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// almaty and istanbul are two zones with DIFFERENT midnights (UTC+5 / UTC+3),
// which is the whole point of the venue-local day boundary.
const (
	tzAlmaty   = "Asia/Almaty"
	tzIstanbul = "Europe/Istanbul"
)

// seedVenue registers a venue with a destination, a timezone and a set of owed
// ledger entries, each with a creation instant so the day boundary is real.
func seedVenue(t *testing.T, h *harness, tz string, entries ...domain.OwedEntry) uuid.UUID {
	t.Helper()
	rid := uuid.New()
	if err := h.dest.Upsert(context.Background(), &domain.PayoutDestination{
		RestaurantID: rid, Provider: domain.ProviderFreedomPay,
		Method: domain.PayoutMethodFreedomPayCardToken, Token: uuid.NewString(),
	}); err != nil {
		t.Fatalf("seed destination: %v", err)
	}
	bal := domain.OwedBalance{RestaurantID: rid, Currency: domain.CurrencyKZT}
	for _, e := range entries {
		bal.AmountMinor += e.AmountSignedMinor
		bal.Entries = append(bal.Entries, e)
	}
	h.owed.byRestaurant[rid] = []domain.OwedBalance{bal}
	h.owed.ids = append(h.owed.ids, rid)
	h.venues.tz[rid] = tz
	return rid
}

// owedAt builds one owed ledger entry created at a given instant.
//
// The instant is recorded twice on purpose, because it plays two different
// roles: entryTimes drives the fake's period CUTOFF (which entries are in scope
// for the day being settled), while OwedEntry.CreatedAt is the AGE the real
// repository returns and the max-hold rule measures against.
func owedAt(h *harness, amountMinor int64, at time.Time) domain.OwedEntry {
	id := uuid.New()
	if h.owed.entryTimes == nil {
		h.owed.entryTimes = map[uuid.UUID]time.Time{}
	}
	h.owed.entryTimes[id] = at
	return domain.OwedEntry{
		LedgerEntryID: id, AmountSignedMinor: amountMinor,
		Currency: domain.CurrencyKZT, CreatedAt: at,
	}
}

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("timezone database unavailable for %s: %v", name, err)
	}
	return loc
}

// ---------------------------------------------------------------------------
// 1. One payout per venue per day
// ---------------------------------------------------------------------------

// TestDaily_OnePayoutPerVenuePerDay is the core idempotency guarantee: running
// the pass twice on the same day must not pay the venue twice, and the payout
// must carry the venue-local period it settled.
//
// Note which guard does the work here: the SECOND pass finds nothing owed
// because the claim table already owns those ledger entries. The period index
// is what covers the case the claim table cannot —
// TestDaily_LateLedgerEntryDoesNotCreateASecondPayoutForTheDay isolates it, and
// that is the test that fails when the period guard is removed.
func TestDaily_OnePayoutPerVenuePerDay(t *testing.T) {
	h := newHarness()
	almaty := mustLoad(t, tzAlmaty)
	// 2026-07-25 09:00 Almaty — yesterday (24.07) has ended.
	now := time.Date(2026, 7, 25, 9, 0, 0, 0, almaty)
	yesterday := time.Date(2026, 7, 24, 20, 0, 0, 0, almaty)

	rid := seedVenue(t, h, tzAlmaty, owedAt(h, 2_000_000, yesterday))
	d := h.daily(DailyConfig{}, now)

	first, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("first tick: %v", err)
	}
	if first.Generated != 1 {
		t.Fatalf("expected 1 payout on the first pass, got %d", first.Generated)
	}

	second, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if second.Generated != 0 {
		t.Fatalf("second pass generated %d payouts for the same day — the venue was paid twice", second.Generated)
	}

	list, _ := h.payouts.List(context.Background(), rid, 100)
	if len(list) != 1 {
		t.Fatalf("expected exactly one payout for the venue-day, got %d", len(list))
	}
	if list[0].PeriodDate == nil {
		t.Fatal("a scheduled payout must carry the venue-local period date")
	}
	if got := list[0].PeriodDate.Format(time.DateOnly); got != "2026-07-24" {
		t.Fatalf("expected period 2026-07-24 (the last completed Almaty day), got %s", got)
	}
}

// TestDaily_LateLedgerEntryDoesNotCreateASecondPayoutForTheDay isolates what
// the period guard — and ONLY the period guard — protects.
//
// The claim table (UNIQUE(ledger_entry_id)) already stops the same money being
// paid twice, so a plain second tick finds nothing owed and generates nothing
// even without a period constraint. The case it does NOT cover: a ledger entry
// that appears AFTER the day was settled but is dated INSIDE it — a webhook
// that landed late, a reconciled capture. Those are genuinely unclaimed, and
// without uq_payouts_venue_period they would produce a SECOND payout for a day
// the venue has already been paid for.
//
// MUTATION CHECK: removing the uq_payouts_venue_period mirror from
// fakePayouts.Create makes this test fail with 2 payouts for 2026-07-24.
func TestDaily_LateLedgerEntryDoesNotCreateASecondPayoutForTheDay(t *testing.T) {
	h := newHarness()
	almaty := mustLoad(t, tzAlmaty)
	now := time.Date(2026, 7, 25, 9, 0, 0, 0, almaty)

	rid := seedVenue(t, h, tzAlmaty, owedAt(h, 2_000_000, time.Date(2026, 7, 24, 12, 0, 0, 0, almaty)))
	d := h.daily(DailyConfig{}, now)
	if _, err := d.Tick(context.Background()); err != nil {
		t.Fatalf("first tick: %v", err)
	}

	// A capture for the 24th arrives late — after that day was already settled.
	late := owedAt(h, 3_000_000, time.Date(2026, 7, 24, 22, 0, 0, 0, almaty))
	bal := h.owed.byRestaurant[rid][0]
	bal.Entries = append(bal.Entries, late)
	bal.AmountMinor += late.AmountSignedMinor
	h.owed.byRestaurant[rid] = []domain.OwedBalance{bal}

	res, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if res.Generated != 0 {
		t.Fatalf("a late entry dated inside an already-settled day must NOT create a second payout, got %d", res.Generated)
	}
	list, _ := h.payouts.List(context.Background(), rid, 10)
	if len(list) != 1 {
		t.Fatalf("expected still exactly one payout for 2026-07-24, got %d", len(list))
	}
	if h.items.isClaimed(late.LedgerEntryID) {
		t.Fatal("the late entry must stay unclaimed so the NEXT day pays it")
	}

	// And it is paid the next day — never lost.
	next := time.Date(2026, 7, 26, 9, 0, 0, 0, almaty)
	d2 := h.daily(DailyConfig{}, next)
	if _, err := d2.Tick(context.Background()); err != nil {
		t.Fatalf("next-day tick: %v", err)
	}
	list, _ = h.payouts.List(context.Background(), rid, 10)
	if len(list) != 2 {
		t.Fatalf("expected the late money to be paid on the next day, got %d payouts", len(list))
	}
}

// TestDaily_ConcurrentTicksPayOnce runs two passes at the same instant, the way
// two worker replicas would. Exactly one payout must exist afterwards —
// whichever of the two DB guards (the claim table or the period index) wins the
// race first. TestPayouts_ConcurrentInsertsForTheSameDay in the postgres layer
// pins the period index specifically, against a real database.
func TestDaily_ConcurrentTicksPayOnce(t *testing.T) {
	h := newHarness()
	almaty := mustLoad(t, tzAlmaty)
	now := time.Date(2026, 7, 25, 9, 0, 0, 0, almaty)

	rid := seedVenue(t, h, tzAlmaty, owedAt(h, 5_000_000, time.Date(2026, 7, 24, 12, 0, 0, 0, almaty)))
	d := h.daily(DailyConfig{}, now)

	var wg sync.WaitGroup
	var mu sync.Mutex
	generated := 0
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := d.Tick(context.Background())
			if err != nil {
				t.Errorf("concurrent tick: %v", err)
				return
			}
			mu.Lock()
			generated += res.Generated
			mu.Unlock()
		}()
	}
	wg.Wait()

	if generated != 1 {
		t.Fatalf("two concurrent passes generated %d payouts, expected exactly 1", generated)
	}
	list, _ := h.payouts.List(context.Background(), rid, 100)
	if len(list) != 1 {
		t.Fatalf("expected one payout row after a concurrent race, got %d", len(list))
	}
}

// ---------------------------------------------------------------------------
// 2. Venue-local day boundary
// ---------------------------------------------------------------------------

// TestDaily_PeriodIsTheVenueLocalDay pins the boundary to the VENUE's zone.
// At 2026-07-25 01:30 Almaty (= 2026-07-24 22:30 Istanbul) the Almaty venue's
// 24.07 has ended, while the Istanbul venue is still IN 24.07 and must settle
// 23.07 instead.
func TestDaily_PeriodIsTheVenueLocalDay(t *testing.T) {
	almaty := mustLoad(t, tzAlmaty)
	istanbul := mustLoad(t, tzIstanbul)
	now := time.Date(2026, 7, 25, 1, 30, 0, 0, almaty)

	h := newHarness()
	// Money earned on 23.07 in both zones, so both venues have something to pay.
	ridAlmaty := seedVenue(t, h, tzAlmaty, owedAt(h, 3_000_000, time.Date(2026, 7, 23, 12, 0, 0, 0, almaty)))
	ridIstanbul := seedVenue(t, h, tzIstanbul, owedAt(h, 3_000_000, time.Date(2026, 7, 23, 12, 0, 0, 0, istanbul)))

	d := h.daily(DailyConfig{}, now)
	if _, err := d.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	period := func(rid uuid.UUID) domain.Payout {
		t.Helper()
		list, _ := h.payouts.List(context.Background(), rid, 10)
		if len(list) != 1 {
			t.Fatalf("expected one payout for %s, got %d", rid, len(list))
		}
		return list[0]
	}

	pa := period(ridAlmaty)
	if got := pa.PeriodDate.Format(time.DateOnly); got != "2026-07-24" {
		t.Fatalf("Almaty venue: expected period 2026-07-24, got %s", got)
	}
	pi := period(ridIstanbul)
	if got := pi.PeriodDate.Format(time.DateOnly); got != "2026-07-23" {
		t.Fatalf("Istanbul venue: expected period 2026-07-23 (its 24.07 has not ended yet), got %s", got)
	}
	// The cutoff instants must be the two zones' own midnights, three hours apart.
	if !pa.PeriodEndAt.Equal(time.Date(2026, 7, 25, 0, 0, 0, 0, almaty)) {
		t.Fatalf("Almaty cutoff is not the local midnight: %s", pa.PeriodEndAt)
	}
	if !pi.PeriodEndAt.Equal(time.Date(2026, 7, 24, 0, 0, 0, 0, istanbul)) {
		t.Fatalf("Istanbul cutoff is not the local midnight: %s", pi.PeriodEndAt)
	}
}

// TestDaily_MoneyAfterLocalMidnightWaitsForTheNextDay proves the cutoff is
// applied, not merely stored: money captured after the venue's local midnight
// belongs to the next period.
func TestDaily_MoneyAfterLocalMidnightWaitsForTheNextDay(t *testing.T) {
	h := newHarness()
	almaty := mustLoad(t, tzAlmaty)
	now := time.Date(2026, 7, 25, 9, 0, 0, 0, almaty)

	yesterday := owedAt(h, 2_000_000, time.Date(2026, 7, 24, 23, 0, 0, 0, almaty))
	today := owedAt(h, 9_000_000, time.Date(2026, 7, 25, 0, 30, 0, 0, almaty))
	rid := seedVenue(t, h, tzAlmaty, yesterday, today)

	d := h.daily(DailyConfig{}, now)
	if _, err := d.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	list, _ := h.payouts.List(context.Background(), rid, 10)
	if len(list) != 1 {
		t.Fatalf("expected one payout, got %d", len(list))
	}
	if list[0].GrossAmountMinor != 2_000_000 {
		t.Fatalf("payout must cover only the finished local day (2 000 000), got %d — today's money leaked in",
			list[0].GrossAmountMinor)
	}
	items, _ := h.items.ListByPayout(context.Background(), list[0].ID)
	if len(items) != 1 || items[0].LedgerEntryID != yesterday.LedgerEntryID {
		t.Fatalf("expected only yesterday's ledger entry claimed, got %v", items)
	}
}

// TestLastCompletedLocalDay_SurvivesDST checks the boundary arithmetic across a
// spring-forward, where a naive now.AddDate(0,0,-1) or a -24h step can land on
// the wrong calendar date. Europe/Istanbul has no DST today, so this uses a
// zone that does.
func TestLastCompletedLocalDay_SurvivesDST(t *testing.T) {
	berlin := mustLoad(t, "Europe/Berlin")
	// 2026-03-29 is the spring-forward day in Berlin (02:00 -> 03:00, a 23h
	// day). At 10:00 on the 30th the last completed day is the 29th.
	now := time.Date(2026, 3, 30, 10, 0, 0, 0, berlin)
	p := lastCompletedLocalDay(now, berlin)
	if got := p.Date.Format(time.DateOnly); got != "2026-03-29" {
		t.Fatalf("expected period 2026-03-29 across the DST shift, got %s", got)
	}
	if !p.EndAt.Equal(time.Date(2026, 3, 30, 0, 0, 0, 0, berlin)) {
		t.Fatalf("expected the cutoff at local midnight of the 30th, got %s", p.EndAt)
	}
}

// ---------------------------------------------------------------------------
// 3. Roll-over
// ---------------------------------------------------------------------------

// TestDaily_BelowMinimumRollsOverAndIsNeverLost is the small-payout guard: a
// balance under the minimum is not paid, nothing is claimed, and the SAME money
// is paid in full the next day together with that day's takings.
//
// MUTATION CHECK: deleting the MinPayoutMinor branch in runVenue makes this
// test fail on the first pass (a payout is generated).
func TestDaily_BelowMinimumRollsOverAndIsNeverLost(t *testing.T) {
	h := newHarness()
	almaty := mustLoad(t, tzAlmaty)
	day1 := time.Date(2026, 7, 25, 9, 0, 0, 0, almaty)

	// 400 ₸ on 24.07 — far below the 10 000 ₸ default minimum, and a payout of
	// it would cost a 300 ₸ floor, i.e. 75% of the money.
	small := owedAt(h, 40_000, time.Date(2026, 7, 24, 18, 0, 0, 0, almaty))
	rid := seedVenue(t, h, tzAlmaty, small)

	d := h.daily(DailyConfig{}, day1)
	res, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("day 1 tick: %v", err)
	}
	if res.Generated != 0 || res.RolledOver != 1 {
		t.Fatalf("expected the balance to roll over, got generated=%d rolled=%d", res.Generated, res.RolledOver)
	}
	if h.items.isClaimed(small.LedgerEntryID) {
		t.Fatal("rolled-over money must stay UNCLAIMED, otherwise it can never be paid")
	}

	// Day 2: 25.07 brings 12 000 ₸. The pass must now pay 12 400 ₸ — the new
	// money AND the rolled-over 400 ₸, each exactly once.
	big := owedAt(h, 1_200_000, time.Date(2026, 7, 25, 14, 0, 0, 0, almaty))
	bal := h.owed.byRestaurant[rid][0]
	bal.Entries = append(bal.Entries, big)
	bal.AmountMinor += big.AmountSignedMinor
	h.owed.byRestaurant[rid] = []domain.OwedBalance{bal}

	day2 := time.Date(2026, 7, 26, 9, 0, 0, 0, almaty)
	d2 := h.daily(DailyConfig{}, day2)
	if _, err := d2.Tick(context.Background()); err != nil {
		t.Fatalf("day 2 tick: %v", err)
	}

	list, _ := h.payouts.List(context.Background(), rid, 10)
	if len(list) != 1 {
		t.Fatalf("expected one payout on day 2, got %d", len(list))
	}
	if list[0].GrossAmountMinor != 1_240_000 {
		t.Fatalf("expected the rolled-over 400 ₸ to be paid with day 2 (1 240 000), got %d",
			list[0].GrossAmountMinor)
	}
	items, _ := h.items.ListByPayout(context.Background(), list[0].ID)
	if len(items) != 2 {
		t.Fatalf("expected both ledger entries claimed exactly once, got %d", len(items))
	}
}

// ---------------------------------------------------------------------------
// 4. A failed send does not consume the period
// ---------------------------------------------------------------------------

// TestDaily_FailedSendDoesNotConsumeThePeriod: a declined payout releases both
// its claimed ledger entries and its venue-day, so the next pass can retry the
// same period instead of the venue silently losing a day's money.
//
// MUTATION CHECK: making the fake's period guard ignore PayoutFailed (i.e.
// dropping `WHERE status <> 'failed'` from the index) makes this test fail —
// the retry is refused and the venue is never paid.
func TestDaily_FailedSendDoesNotConsumeThePeriod(t *testing.T) {
	h := newHarness()
	almaty := mustLoad(t, tzAlmaty)
	now := time.Date(2026, 7, 25, 9, 0, 0, 0, almaty)

	rid := seedVenue(t, h, tzAlmaty, owedAt(h, 4_000_000, time.Date(2026, 7, 24, 12, 0, 0, 0, almaty)))

	declines := true
	h.gw.payoutFn = func(req domain.PayoutRequest) (*domain.GatewayPayout, error) {
		if declines {
			return nil, domain.ErrProviderDeclined
		}
		return &domain.GatewayPayout{ProviderRef: "ok", Status: domain.PayoutPaid, Amount: req.Amount}, nil
	}

	d := h.daily(DailyConfig{SendEnabled: true}, now)
	if _, err := d.Tick(context.Background()); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	list, _ := h.payouts.List(context.Background(), rid, 10)
	if len(list) != 1 || list[0].Status != domain.PayoutFailed {
		t.Fatalf("expected one failed payout after a decline, got %+v", list)
	}

	// The retry: the same period, now succeeding.
	declines = false
	d2 := h.daily(DailyConfig{SendEnabled: true}, now)
	res, err := d2.Tick(context.Background())
	if err != nil {
		t.Fatalf("retry tick: %v", err)
	}
	if res.Generated != 1 {
		t.Fatalf("a failed payout must free its period for a retry, got generated=%d skipped=%d",
			res.Generated, res.Skipped)
	}
	list, _ = h.payouts.List(context.Background(), rid, 10)
	paid := 0
	for _, p := range list {
		if p.Status == domain.PayoutPaid {
			paid++
		}
	}
	if paid != 1 {
		t.Fatalf("expected exactly one PAID payout after the retry, got %d of %d rows", paid, len(list))
	}
}

// TestDaily_UnknownOutcomeConsumesThePeriod is the other half of the same rule:
// an UNKNOWN acquirer answer is not a failure. The money may already be moving,
// so the payout stays `sent`, keeps its period, and is left to the reconciler —
// re-generating it would be how a venue gets paid twice.
func TestDaily_UnknownOutcomeConsumesThePeriod(t *testing.T) {
	h := newHarness()
	almaty := mustLoad(t, tzAlmaty)
	now := time.Date(2026, 7, 25, 9, 0, 0, 0, almaty)

	rid := seedVenue(t, h, tzAlmaty, owedAt(h, 4_000_000, time.Date(2026, 7, 24, 12, 0, 0, 0, almaty)))
	h.gw.payoutFn = func(domain.PayoutRequest) (*domain.GatewayPayout, error) {
		return nil, domain.ErrProviderOutcomeUnknown
	}

	d := h.daily(DailyConfig{SendEnabled: true}, now)
	if _, err := d.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	res, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if res.Generated != 0 {
		t.Fatalf("an unknown outcome must NOT free the period, got generated=%d", res.Generated)
	}
	list, _ := h.payouts.List(context.Background(), rid, 10)
	if len(list) != 1 || list[0].Status != domain.PayoutSent {
		t.Fatalf("expected one payout left `sent` for the reconciler, got %+v", list)
	}
}

// ---------------------------------------------------------------------------
// 5. Fee, bearer policy and the ledger
// ---------------------------------------------------------------------------

// TestDaily_PlatformAbsorbsFeeByDefault: the default policy must never shrink
// the venue's payout below what its statement said.
func TestDaily_PlatformAbsorbsFeeByDefault(t *testing.T) {
	h := newHarness()
	almaty := mustLoad(t, tzAlmaty)
	now := time.Date(2026, 7, 25, 9, 0, 0, 0, almaty)

	rid := seedVenue(t, h, tzAlmaty, owedAt(h, 1_000_000, time.Date(2026, 7, 24, 12, 0, 0, 0, almaty)))
	d := h.daily(DailyConfig{SendEnabled: true}, now)
	if _, err := d.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	list, _ := h.payouts.List(context.Background(), rid, 10)
	p := list[0]
	if p.AmountMinor != 1_000_000 {
		t.Fatalf("platform-absorbs: the venue must receive the full 10 000 ₸, got %d", p.AmountMinor)
	}
	// 10 000 ₸ × 1.9% = 190 ₸, below the 300 ₸ floor, so the floor applies.
	if p.FeeMinor != 30_000 {
		t.Fatalf("expected the 300 ₸ floor as the fee, got %d", p.FeeMinor)
	}
	if p.FeeBearer != domain.PayoutFeeBearerPlatform {
		t.Fatalf("expected the platform to bear the fee by default, got %q", p.FeeBearer)
	}

	// The ledger mirrors it: the venue is debited what it received, the
	// PLATFORM is debited the fee.
	entries, _ := h.ledger.ListByPayout(context.Background(), p.ID)
	if err := domain.ValidatePayoutLedgerBalance(entries); err != nil {
		t.Fatalf("payout ledger does not balance: %v", err)
	}
	var venueDebit, platformFee int64
	for _, e := range entries {
		switch {
		case e.Account == domain.AccountRestaurant && e.EntryType == domain.EntrySettlement:
			venueDebit += e.AmountMinor
		case e.Account == domain.AccountPlatform && e.EntryType == domain.EntryPayoutFee:
			platformFee += e.AmountMinor
		}
	}
	if venueDebit != 1_000_000 || platformFee != 30_000 {
		t.Fatalf("expected venue debit 1 000 000 and platform fee 30 000, got %d / %d", venueDebit, platformFee)
	}
}

// TestDaily_VenueBearsFeeIsDeducted covers the other policy end to end.
func TestDaily_VenueBearsFeeIsDeducted(t *testing.T) {
	h := newHarnessWithConfig(Config{FeeBearer: domain.PayoutFeeBearerVenue})
	almaty := mustLoad(t, tzAlmaty)
	now := time.Date(2026, 7, 25, 9, 0, 0, 0, almaty)

	rid := seedVenue(t, h, tzAlmaty, owedAt(h, 1_000_000, time.Date(2026, 7, 24, 12, 0, 0, 0, almaty)))
	d := h.daily(DailyConfig{SendEnabled: true}, now)
	if _, err := d.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	list, _ := h.payouts.List(context.Background(), rid, 10)
	p := list[0]
	if p.GrossAmountMinor != 1_000_000 || p.FeeMinor != 30_000 || p.AmountMinor != 970_000 {
		t.Fatalf("venue-bears: expected gross 1 000 000, fee 30 000, transferred 970 000; got %d / %d / %d",
			p.GrossAmountMinor, p.FeeMinor, p.AmountMinor)
	}
	entries, _ := h.ledger.ListByPayout(context.Background(), p.ID)
	if err := domain.ValidatePayoutLedgerBalance(entries); err != nil {
		t.Fatalf("payout ledger does not balance: %v", err)
	}
	var venueTotal int64
	for _, e := range entries {
		if e.Account == domain.AccountRestaurant && e.Direction == domain.DirectionDebit {
			venueTotal += e.AmountMinor
		}
	}
	if venueTotal != 1_000_000 {
		t.Fatalf("venue-bears: the venue's balance must drop by the full gross, got %d", venueTotal)
	}
}

// TestDaily_LedgerIsBookedOnlyOnce: a repeated "mark paid" must not book the
// acquirer's fee a second time.
func TestDaily_LedgerIsBookedOnlyOnce(t *testing.T) {
	h := newHarness()
	almaty := mustLoad(t, tzAlmaty)
	now := time.Date(2026, 7, 25, 9, 0, 0, 0, almaty)

	rid := seedVenue(t, h, tzAlmaty, owedAt(h, 2_000_000, time.Date(2026, 7, 24, 12, 0, 0, 0, almaty)))
	d := h.daily(DailyConfig{SendEnabled: true}, now)
	if _, err := d.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	list, _ := h.payouts.List(context.Background(), rid, 10)
	p := list[0]

	// Replay the send: the payout is already paid, so nothing may be booked.
	if _, err := h.uc.SendPayout(context.Background(), superadmin(), p.ID); err != nil {
		t.Fatalf("replayed send: %v", err)
	}
	entries, _ := h.ledger.ListByPayout(context.Background(), p.ID)
	if len(entries) != 4 {
		t.Fatalf("expected 4 ledger lines (settlement pair + fee pair), got %d", len(entries))
	}
}

// ---------------------------------------------------------------------------
// 6. Housekeeping paths
// ---------------------------------------------------------------------------

// TestDaily_VenueWithoutDestinationIsSkippedNotFailed: a venue that never told
// us where to send its money must not stop the pass, and its money must stay
// owed.
func TestDaily_VenueWithoutDestinationIsSkippedNotFailed(t *testing.T) {
	h := newHarness()
	almaty := mustLoad(t, tzAlmaty)
	now := time.Date(2026, 7, 25, 9, 0, 0, 0, almaty)

	entry := owedAt(h, 5_000_000, time.Date(2026, 7, 24, 12, 0, 0, 0, almaty))
	noDest := uuid.New()
	h.owed.byRestaurant[noDest] = []domain.OwedBalance{{
		RestaurantID: noDest, Currency: domain.CurrencyKZT,
		AmountMinor: entry.AmountSignedMinor, Entries: []domain.OwedEntry{entry},
	}}
	h.owed.ids = append(h.owed.ids, noDest)
	h.venues.tz[noDest] = tzAlmaty
	// A healthy venue after it in the list must still be paid.
	ok := seedVenue(t, h, tzAlmaty, owedAt(h, 5_000_000, time.Date(2026, 7, 24, 12, 0, 0, 0, almaty)))

	d := h.daily(DailyConfig{}, now)
	res, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Skipped != 1 || res.Generated != 1 {
		t.Fatalf("expected 1 skipped and 1 generated, got skipped=%d generated=%d", res.Skipped, res.Generated)
	}
	if h.items.isClaimed(entry.LedgerEntryID) {
		t.Fatal("a venue with no destination must keep its money owed, not claimed")
	}
	if list, _ := h.payouts.List(context.Background(), ok, 10); len(list) != 1 {
		t.Fatalf("the healthy venue must still be paid, got %d payouts", len(list))
	}
}

// TestDaily_SendDisabledGeneratesButMovesNoMoney is the deployment default.
func TestDaily_SendDisabledGeneratesButMovesNoMoney(t *testing.T) {
	h := newHarness()
	almaty := mustLoad(t, tzAlmaty)
	now := time.Date(2026, 7, 25, 9, 0, 0, 0, almaty)

	rid := seedVenue(t, h, tzAlmaty, owedAt(h, 2_000_000, time.Date(2026, 7, 24, 12, 0, 0, 0, almaty)))
	d := h.daily(DailyConfig{SendEnabled: false}, now)
	if _, err := d.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if h.gw.payoutCalls != 0 {
		t.Fatalf("send is disabled, the acquirer must not be called (%d calls)", h.gw.payoutCalls)
	}
	list, _ := h.payouts.List(context.Background(), rid, 10)
	if len(list) != 1 || list[0].Status != domain.PayoutPending {
		t.Fatalf("expected one PENDING payout, got %+v", list)
	}
}

// Owner decision (2026-07-25): the hold cap must never send a payout the fee
// would eat. 250 ₸ held past the limit cannot even cover the 300 ₸ transfer, so
// it keeps waiting and is counted as STUCK — the number an operator is meant to
// act on, rather than money quietly destroyed by its own transfer.
func TestDaily_HeldTooLongButTooSmallToPayIsNotForced(t *testing.T) {
	h := newHarness()
	almaty := mustLoad(t, tzAlmaty)
	now := time.Date(2026, 7, 25, 9, 0, 0, 0, almaty)

	// 250 ₸, twenty days old: past any hold cap, below the 300 ₸ fee floor.
	rid := seedVenue(t, h, tzAlmaty, owedAt(h, 25_000, time.Date(2026, 7, 5, 12, 0, 0, 0, almaty)))

	d := h.daily(DailyConfig{}, now)
	res, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Generated != 0 || res.Forced != 0 {
		t.Fatalf("an uneconomic balance was paid anyway: generated=%d forced=%d", res.Generated, res.Forced)
	}
	if res.Stuck != 1 {
		t.Fatalf("stuck = %d, want 1 — money nobody can pay economically must be surfaced, not silently held", res.Stuck)
	}
	if list, _ := h.payouts.List(context.Background(), rid, 10); len(list) != 0 {
		t.Fatalf("expected no payout row, got %d", len(list))
	}
}

// The rule is "smaller than the fee", not "small": a balance the fee does not
// swallow is still force-paid when it gets old, which is the whole point of the
// hold cap.
func TestDaily_HeldTooLongAndWorthPayingIsStillForced(t *testing.T) {
	h := newHarness()
	almaty := mustLoad(t, tzAlmaty)
	now := time.Date(2026, 7, 25, 9, 0, 0, 0, almaty)

	// 5 000 ₸: below the 10 000 ₸ threshold, comfortably above the 300 ₸ fee.
	rid := seedVenue(t, h, tzAlmaty, owedAt(h, 500_000, time.Date(2026, 7, 5, 12, 0, 0, 0, almaty)))

	d := h.daily(DailyConfig{}, now)
	res, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Forced != 1 || res.Stuck != 0 {
		t.Fatalf("forced=%d stuck=%d, want forced=1 stuck=0", res.Forced, res.Stuck)
	}
	list, _ := h.payouts.List(context.Background(), rid, 10)
	if len(list) != 1 || !list[0].ForcedByAge {
		t.Fatalf("expected one payout marked forced_by_age, got %+v", list)
	}
}
