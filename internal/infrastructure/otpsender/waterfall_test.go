package otpsender

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"backend-core/internal/domain"
)

// fakeChannel is a Channel that records its calls and answers with a scripted
// error. It is the whole test double surface for the waterfall: everything the
// waterfall does is ordering plus reacting to an error.
type fakeChannel struct {
	name string
	err  error
	// messageID is what the provider would answer with on success.
	messageID string
	calls     int
	// block makes the channel hang until its context is done, so the per-channel
	// timeout can be exercised without a real network.
	block bool
}

func (f *fakeChannel) Name() string { return f.name }

func (f *fakeChannel) Send(ctx context.Context, _, _ string) (string, error) {
	f.calls++
	if f.block {
		<-ctx.Done()
		return "", ctx.Err()
	}
	if f.err != nil {
		return "", f.err
	}
	return f.messageID, nil
}

func testLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

const (
	testPhone = "+77075552233"
	testCode  = "482913"
)

func TestWaterfallOrdering(t *testing.T) {
	unreachable := fmt.Errorf("%w: not here", ErrChannelUnavailable)

	tests := []struct {
		name string
		// errs maps channel name -> error it answers with (nil = it delivers).
		errs      map[string]error
		hint      domain.OTPSendHint
		wantOrder []string // channels expected to be TRIED, in order
		wantChan  string
		wantErr   error
	}{
		{
			name:      "default order tries telegram first",
			errs:      map[string]error{},
			wantOrder: []string{domain.OTPChannelTelegram},
			wantChan:  domain.OTPChannelTelegram,
		},
		{
			// Telegram's pre-check said the number is not on Telegram: the
			// cheapest possible "no", and the reason it is first.
			name:      "telegram cannot deliver — whatsapp takes it",
			errs:      map[string]error{domain.OTPChannelTelegram: unreachable},
			wantOrder: []string{domain.OTPChannelTelegram, domain.OTPChannelWhatsApp},
			wantChan:  domain.OTPChannelWhatsApp,
		},
		{
			// A transport-level fault (timeout, 502, unparseable answer) is not
			// distinguishable from a refusal to the guest: same fallback.
			name:      "telegram errors — whatsapp takes it",
			errs:      map[string]error{domain.OTPChannelTelegram: errors.New("gatewayapi: http 502")},
			wantOrder: []string{domain.OTPChannelTelegram, domain.OTPChannelWhatsApp},
			wantChan:  domain.OTPChannelWhatsApp,
		},
		{
			// The real 2026-07-25 case: the Gateway balance is 0. It must cost
			// the guest one extra hop, not a 500.
			name:      "telegram out of balance — whatsapp takes it",
			errs:      map[string]error{domain.OTPChannelTelegram: fmt.Errorf("%w: BALANCE_NOT_ENOUGH", ErrChannelUnavailable)},
			wantOrder: []string{domain.OTPChannelTelegram, domain.OTPChannelWhatsApp},
			wantChan:  domain.OTPChannelWhatsApp,
		},
		{
			name: "whatsapp fails after telegram — sms is the last resort",
			errs: map[string]error{
				domain.OTPChannelTelegram: unreachable,
				domain.OTPChannelWhatsApp: errors.New("meta: rejected (http 400, code 132000)"),
			},
			wantOrder: []string{
				domain.OTPChannelTelegram, domain.OTPChannelWhatsApp, domain.OTPChannelSMS,
			},
			wantChan: domain.OTPChannelSMS,
		},
		{
			name:      "remembered channel is tried first",
			errs:      map[string]error{},
			hint:      domain.OTPSendHint{Prefer: domain.OTPChannelSMS},
			wantOrder: []string{domain.OTPChannelSMS},
			wantChan:  domain.OTPChannelSMS,
		},
		{
			// The point of the whole memory: a stale preference must cost one
			// attempt, never a login.
			name: "remembered channel unavailable falls through to the rest",
			errs: map[string]error{domain.OTPChannelSMS: unreachable},
			hint: domain.OTPSendHint{Prefer: domain.OTPChannelSMS},
			wantOrder: []string{
				domain.OTPChannelSMS, domain.OTPChannelTelegram,
			},
			wantChan: domain.OTPChannelTelegram,
		},
		{
			name:      "unknown remembered channel is ignored, not fatal",
			errs:      map[string]error{},
			hint:      domain.OTPSendHint{Prefer: "carrier_pigeon"},
			wantOrder: []string{domain.OTPChannelTelegram},
			wantChan:  domain.OTPChannelTelegram,
		},
		{
			name:      "deprioritized channel goes last",
			errs:      map[string]error{},
			hint:      domain.OTPSendHint{Deprioritize: []string{domain.OTPChannelTelegram}},
			wantOrder: []string{domain.OTPChannelWhatsApp},
			wantChan:  domain.OTPChannelWhatsApp,
		},
		{
			// Fresh evidence (an unverified code sitting on that channel right
			// now) beats the older memory of a successful login.
			name:      "deprioritize overrules prefer for the same channel",
			errs:      map[string]error{},
			hint:      domain.OTPSendHint{Prefer: domain.OTPChannelWhatsApp, Deprioritize: []string{domain.OTPChannelWhatsApp}},
			wantOrder: []string{domain.OTPChannelTelegram},
			wantChan:  domain.OTPChannelTelegram,
		},
		{
			// A demoted channel is moved, never removed: if it is the only one
			// left it is still tried, because no login beats a slow login.
			name: "deprioritized channel is still tried when everything else fails",
			errs: map[string]error{
				domain.OTPChannelTelegram: unreachable,
				domain.OTPChannelWhatsApp: unreachable,
			},
			hint: domain.OTPSendHint{Deprioritize: []string{domain.OTPChannelSMS}},
			wantOrder: []string{
				domain.OTPChannelTelegram, domain.OTPChannelWhatsApp, domain.OTPChannelSMS,
			},
			wantChan: domain.OTPChannelSMS,
		},
		{
			name: "all channels down",
			errs: map[string]error{
				domain.OTPChannelTelegram: unreachable,
				domain.OTPChannelWhatsApp: errors.New("meta is having a day"),
				domain.OTPChannelSMS:      errors.New("no balance"),
			},
			wantOrder: []string{
				domain.OTPChannelTelegram, domain.OTPChannelWhatsApp, domain.OTPChannelSMS,
			},
			wantErr: ErrDeliveryFailed,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tg := &fakeChannel{name: domain.OTPChannelTelegram, err: tc.errs[domain.OTPChannelTelegram]}
			wa := &fakeChannel{name: domain.OTPChannelWhatsApp, err: tc.errs[domain.OTPChannelWhatsApp]}
			sms := &fakeChannel{name: domain.OTPChannelSMS, err: tc.errs[domain.OTPChannelSMS]}

			log, _ := testLogger()
			w := NewWaterfall(log, WaterfallConfig{ChannelTimeout: time.Second}, tg, wa, sms)

			channel, err := w.Send(context.Background(), testPhone, testCode, tc.hint)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
			} else {
				if err != nil {
					t.Fatalf("Send: %v", err)
				}
				if channel != tc.wantChan {
					t.Fatalf("channel = %q, want %q", channel, tc.wantChan)
				}
			}

			byName := map[string]*fakeChannel{
				domain.OTPChannelTelegram: tg,
				domain.OTPChannelWhatsApp: wa,
				domain.OTPChannelSMS:      sms,
			}
			tried := map[string]bool{}
			for _, name := range tc.wantOrder {
				tried[name] = true
				if byName[name].calls != 1 {
					t.Errorf("channel %s: calls = %d, want 1", name, byName[name].calls)
				}
			}
			for name, ch := range byName {
				if !tried[name] && ch.calls != 0 {
					t.Errorf("channel %s was tried but should not have been", name)
				}
			}
		})
	}
}

// A deployment with only some credentials present builds a shorter waterfall.
// The guest must still get a code, and a memory pointing at a channel that no
// longer exists must be ignored rather than fatal.
func TestWaterfallWithOneChannelMissingFromConfig(t *testing.T) {
	log, _ := testLogger()
	wa := &fakeChannel{name: domain.OTPChannelWhatsApp, err: errors.New("meta is having a day")}
	sms := &fakeChannel{name: domain.OTPChannelSMS}
	// Telegram has no token in this deployment, so it was never constructed.
	w := NewWaterfall(log, WaterfallConfig{ChannelTimeout: time.Second}, wa, sms)

	if got := w.Channels(); len(got) != 2 {
		t.Fatalf("channels = %v, want the two configured ones", got)
	}
	channel, err := w.Send(context.Background(), testPhone, testCode,
		domain.OTPSendHint{Prefer: domain.OTPChannelTelegram})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if channel != domain.OTPChannelSMS {
		t.Fatalf("channel = %q, want sms", channel)
	}
	if wa.calls != 1 || sms.calls != 1 {
		t.Fatalf("calls: whatsapp=%d sms=%d, want 1 and 1", wa.calls, sms.calls)
	}
}

// The provider's own id is the only handle shared with the provider, so a
// successful send must log it — that is how "the guest says nothing arrived" is
// traced in Meta's or Telegram's dashboard.
func TestWaterfallLogsTheProviderMessageID(t *testing.T) {
	log, buf := testLogger()
	w := NewWaterfall(log, WaterfallConfig{ChannelTimeout: time.Second},
		&fakeChannel{name: domain.OTPChannelWhatsApp, messageID: "wamid.HBgL"},
	)

	if _, err := w.Send(context.Background(), testPhone, testCode, domain.OTPSendHint{}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !strings.Contains(buf.String(), "wamid.HBgL") {
		t.Fatalf("the provider message id is missing from the log: %s", buf.String())
	}
}

// A total failure must be one opaque error. Anything richer — "telegram: not on
// telegram, whatsapp: not on whatsapp" — turns the login endpoint into a free
// "does this number have Telegram/WhatsApp" oracle for anyone with a phone list.
func TestWaterfallTotalFailureLeaksNoChannelDetail(t *testing.T) {
	log, _ := testLogger()
	w := NewWaterfall(log, WaterfallConfig{ChannelTimeout: time.Second},
		&fakeChannel{name: domain.OTPChannelTelegram, err: fmt.Errorf("%w: PHONE_NUMBER_NOT_TELEGRAM", ErrChannelUnavailable)},
		&fakeChannel{name: domain.OTPChannelWhatsApp, err: errors.New("131026 not on whatsapp")},
	)

	_, err := w.Send(context.Background(), testPhone, testCode, domain.OTPSendHint{})
	if !errors.Is(err, ErrDeliveryFailed) {
		t.Fatalf("err = %v, want ErrDeliveryFailed", err)
	}
	for _, leak := range []string{"telegram", "whatsapp", "131026", "NOT_TELEGRAM", testPhone} {
		if strings.Contains(strings.ToLower(err.Error()), strings.ToLower(leak)) {
			t.Fatalf("error %q leaks %q to the caller", err, leak)
		}
	}
}

// The OTP is a bearer credential: unlike the Stub, a REAL send never prints it,
// not even in development. This test is the guard rail — it must fail if the
// scrub in the waterfall (or a channel's promise not to embed the code) is ever
// removed.
func TestWaterfallNeverLogsTheCode(t *testing.T) {
	log, buf := testLogger()
	w := NewWaterfall(log, WaterfallConfig{ChannelTimeout: time.Second},
		// A channel that misbehaves in the worst plausible way: it echoes the
		// whole request, code included, into its error.
		&fakeChannel{name: domain.OTPChannelTelegram, err: fmt.Errorf("provider echoed request: code=%s phone=%s", testCode, testPhone)},
		&fakeChannel{name: domain.OTPChannelSMS},
	)

	channel, err := w.Send(context.Background(), testPhone, testCode, domain.OTPSendHint{})
	if err != nil || channel != domain.OTPChannelSMS {
		t.Fatalf("Send = %q, %v", channel, err)
	}

	out := buf.String()
	if strings.Contains(out, testCode) {
		t.Fatalf("the OTP code reached the log: %s", out)
	}
	if strings.Contains(out, testPhone) {
		t.Fatalf("an unmasked phone reached the log: %s", out)
	}
	if !strings.Contains(out, "+7707***2233") {
		t.Fatalf("the masked phone is missing from the log: %s", out)
	}
}

// A hung provider must cost one timeout, not the login: the channels below it
// still get their turn.
func TestWaterfallChannelTimeoutDoesNotBlockTheNextChannel(t *testing.T) {
	log, _ := testLogger()
	hung := &fakeChannel{name: domain.OTPChannelWhatsApp, block: true}
	sms := &fakeChannel{name: domain.OTPChannelSMS}
	w := NewWaterfall(log, WaterfallConfig{ChannelTimeout: 20 * time.Millisecond}, hung, sms)

	start := time.Now()
	channel, err := w.Send(context.Background(), testPhone, testCode, domain.OTPSendHint{})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if channel != domain.OTPChannelSMS {
		t.Fatalf("channel = %q, want sms", channel)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the hung channel was not capped by the per-channel timeout (took %s)", elapsed)
	}
}

// Per-channel timeouts alone do not bound a login: with enough channels their
// sum would outlive the HTTP server's WriteTimeout. The total budget is what
// guarantees the endpoint still gets to answer the guest.
func TestWaterfallTotalBudgetStopsTheWalk(t *testing.T) {
	log, _ := testLogger()
	first := &fakeChannel{name: domain.OTPChannelTelegram, block: true}
	second := &fakeChannel{name: domain.OTPChannelWhatsApp, block: true}
	third := &fakeChannel{name: domain.OTPChannelSMS}
	w := NewWaterfall(log, WaterfallConfig{
		ChannelTimeout: 20 * time.Millisecond,
		TotalBudget:    30 * time.Millisecond,
	}, first, second, third)

	start := time.Now()
	if _, err := w.Send(context.Background(), testPhone, testCode, domain.OTPSendHint{}); !errors.Is(err, ErrDeliveryFailed) {
		t.Fatalf("err = %v, want ErrDeliveryFailed", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("the walk ran for %s — the total budget did not stop it", elapsed)
	}
	// The budget ran out during the second hung channel, so the third one is
	// never even attempted: no money is spent on a request that can no longer
	// be answered.
	if third.calls != 0 {
		t.Fatalf("sms was tried %d times after the budget expired, want 0", third.calls)
	}
}

// A caller who has already given up (cancelled context) must not have money
// spent on their behalf.
func TestWaterfallStopsOnCancelledContext(t *testing.T) {
	log, _ := testLogger()
	ch := &fakeChannel{name: domain.OTPChannelSMS}
	w := NewWaterfall(log, WaterfallConfig{ChannelTimeout: time.Second}, ch)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := w.Send(ctx, testPhone, testCode, domain.OTPSendHint{}); !errors.Is(err, ErrDeliveryFailed) {
		t.Fatalf("err = %v, want ErrDeliveryFailed", err)
	}
	if ch.calls != 0 {
		t.Fatalf("channel was called %d times on a cancelled context, want 0", ch.calls)
	}
}

// With nothing configured there is no waterfall at all — bootstrap falls back to
// the Stub. Expressed here so the contract is tested and not just documented.
func TestNewWaterfallWithoutChannelsIsNil(t *testing.T) {
	log, _ := testLogger()
	if w := NewWaterfall(log, WaterfallConfig{ChannelTimeout: time.Second}); w != nil {
		t.Fatalf("NewWaterfall() with no channels = %v, want nil", w)
	}
}

func TestSanitize(t *testing.T) {
	err := sanitize(errors.New("boom 482913 for +77075552233"), testCode, testPhone)
	if strings.Contains(err.Error(), testCode) {
		t.Fatalf("sanitize left the code in %q", err)
	}
	if strings.Contains(err.Error(), testPhone) {
		t.Fatalf("sanitize left the raw phone in %q", err)
	}
	if !strings.Contains(err.Error(), "+7707***2233") {
		t.Fatalf("sanitize dropped the masked phone: %q", err)
	}
	if got := sanitize(nil, testCode, testPhone); got != nil {
		t.Fatalf("sanitize(nil) = %v, want nil", got)
	}
}
