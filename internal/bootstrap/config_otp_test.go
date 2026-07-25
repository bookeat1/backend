package bootstrap

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/otpsender"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// baseOTPConfig is a Config with NO OTP credentials at all — each test switches
// on exactly the channels it is about.
func baseOTPConfig() Config {
	var cfg Config
	cfg.App.Environment = "production"
	cfg.Auth.OTPCodeTTL = 5 * time.Minute
	cfg.OTPDelivery.ChannelOrder = []string{
		domain.OTPChannelTelegram, domain.OTPChannelWhatsApp, domain.OTPChannelSMS,
	}
	cfg.OTPDelivery.SendTimeout = 10 * time.Second
	return cfg
}

// A channel exists if and only if its credentials do. This is the whole
// configuration contract: the service must boot and keep working with one, two
// or zero channels present, so a missing token is never an outage.
func TestNewOTPSenderChannelSelection(t *testing.T) {
	withTelegram := func(c *Config) { c.OTPDelivery.TelegramGatewayToken = "tg-token" }
	withWhatsApp := func(c *Config) {
		c.OTPDelivery.WhatsAppAccessToken = "wa-token"
		c.OTPDelivery.WhatsAppPhoneNumberID = "1079483488592272"
		c.OTPDelivery.WhatsAppTemplateName = "bookeat_otp_en"
	}
	withMobizon := func(c *Config) {
		c.OTPDelivery.SMSProvider = "mobizon"
		c.OTPDelivery.MobizonAPIKey = "mobizon-key"
	}

	tests := []struct {
		name string
		set  []func(*Config)
		want []string // expected waterfall order; nil = the Stub sender
	}{
		{
			name: "nothing configured falls back to the stub",
			want: nil,
		},
		{
			name: "all three configured keep the configured order",
			set:  []func(*Config){withTelegram, withWhatsApp, withMobizon},
			want: []string{domain.OTPChannelTelegram, domain.OTPChannelWhatsApp, domain.OTPChannelSMS},
		},
		{
			// The morning of the WhatsApp token arriving: Telegram and SMS keep
			// working, WhatsApp is simply not in the order.
			name: "a channel without credentials is skipped, the rest still route",
			set:  []func(*Config){withTelegram, withMobizon},
			want: []string{domain.OTPChannelTelegram, domain.OTPChannelSMS},
		},
		{
			name: "one channel is enough",
			set:  []func(*Config){withWhatsApp},
			want: []string{domain.OTPChannelWhatsApp},
		},
		{
			// Credentials present but no provider selected: the SMS channel does
			// not exist, because an old key left in the environment must never
			// silently take over the most expensive channel.
			name: "sms credentials without OTP_SMS_PROVIDER do not enable sms",
			set: []func(*Config){withTelegram, func(c *Config) {
				c.OTPDelivery.MobizonAPIKey = "mobizon-key"
			}},
			want: []string{domain.OTPChannelTelegram},
		},
		{
			name: "an unknown provider name disables sms instead of crashing",
			set: []func(*Config){withTelegram, func(c *Config) {
				c.OTPDelivery.SMSProvider = "carrier-pigeon"
				c.OTPDelivery.MobizonAPIKey = "mobizon-key"
			}},
			want: []string{domain.OTPChannelTelegram},
		},
		{
			name: "incomplete whatsapp credentials count as absent",
			set: []func(*Config){withTelegram, func(c *Config) {
				c.OTPDelivery.WhatsAppAccessToken = "wa-token" // no phone id, no template
			}},
			want: []string{domain.OTPChannelTelegram},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseOTPConfig()
			for _, apply := range tc.set {
				apply(&cfg)
			}

			sender := newOTPSender(cfg, discardLogger())
			if tc.want == nil {
				if _, ok := sender.(*otpsender.Stub); !ok {
					t.Fatalf("sender = %T, want the Stub when nothing is configured", sender)
				}
				return
			}
			waterfall, ok := sender.(*otpsender.Waterfall)
			if !ok {
				t.Fatalf("sender = %T, want a Waterfall", sender)
			}
			got := waterfall.Channels()
			if len(got) != len(tc.want) {
				t.Fatalf("channels = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("channels = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// The order is configuration, not a constant: an operator can put SMS first for
// a country where Telegram is blocked, without a deploy.
func TestNewOTPSenderHonoursTheConfiguredOrder(t *testing.T) {
	cfg := baseOTPConfig()
	cfg.OTPDelivery.ChannelOrder = []string{domain.OTPChannelSMS, domain.OTPChannelTelegram}
	cfg.OTPDelivery.TelegramGatewayToken = "tg-token"
	cfg.OTPDelivery.SMSProvider = "mobizon"
	cfg.OTPDelivery.MobizonAPIKey = "mobizon-key"

	waterfall, ok := newOTPSender(cfg, discardLogger()).(*otpsender.Waterfall)
	if !ok {
		t.Fatal("want a Waterfall")
	}
	got := waterfall.Channels()
	if len(got) != 2 || got[0] != domain.OTPChannelSMS || got[1] != domain.OTPChannelTelegram {
		t.Fatalf("channels = %v, want [sms telegram_gateway]", got)
	}
}

// A configured channel left out of OTP_CHANNEL_ORDER (a typo, or a name added
// to the code but not to the env) must still be reachable — silently unusable
// paid credentials are worse than an ugly order.
func TestNewOTPSenderAppendsChannelsMissingFromTheOrder(t *testing.T) {
	cfg := baseOTPConfig()
	cfg.OTPDelivery.ChannelOrder = []string{domain.OTPChannelTelegram}
	cfg.OTPDelivery.TelegramGatewayToken = "tg-token"
	cfg.OTPDelivery.SMSProvider = "mobizon"
	cfg.OTPDelivery.MobizonAPIKey = "mobizon-key"

	waterfall, ok := newOTPSender(cfg, discardLogger()).(*otpsender.Waterfall)
	if !ok {
		t.Fatal("want a Waterfall")
	}
	got := waterfall.Channels()
	if len(got) != 2 || got[0] != domain.OTPChannelTelegram || got[1] != domain.OTPChannelSMS {
		t.Fatalf("channels = %v, want the unlisted sms appended last", got)
	}
}
