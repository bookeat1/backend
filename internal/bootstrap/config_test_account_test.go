package bootstrap

import (
	"strings"
	"testing"
)

// The App Store review account is configuration, not code: it exists only when
// BOTH variables are present, and a half-typed pair must stop the boot instead
// of silently disabling a login the next submission depends on.
func TestTestAccountConfig(t *testing.T) {
	t.Run("absent by default", func(t *testing.T) {
		cfg, err := NewConfig()
		if err != nil {
			t.Fatalf("NewConfig: %v", err)
		}
		if cfg.Auth.TestAccountPhone != "" || cfg.Auth.TestAccountCode != "" {
			t.Errorf("expected no test account, got phone=%q code=%q",
				cfg.Auth.TestAccountPhone, cfg.Auth.TestAccountCode)
		}
	})

	t.Run("both set are carried through, whitespace trimmed", func(t *testing.T) {
		t.Setenv("AUTH_TEST_ACCOUNT_PHONE", "  +7 777 000 00 00 ")
		t.Setenv("AUTH_TEST_ACCOUNT_CODE", " 123456 ")

		cfg, err := NewConfig()
		if err != nil {
			t.Fatalf("NewConfig: %v", err)
		}
		if cfg.Auth.TestAccountPhone != "+7 777 000 00 00" {
			t.Errorf("phone = %q", cfg.Auth.TestAccountPhone)
		}
		if cfg.Auth.TestAccountCode != "123456" {
			t.Errorf("code = %q", cfg.Auth.TestAccountCode)
		}
	})

	t.Run("half a pair refuses to boot", func(t *testing.T) {
		for _, tc := range []struct{ name, phone, code string }{
			{"phone without code", "+77770000000", ""},
			{"code without phone", "", "123456"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Setenv("AUTH_TEST_ACCOUNT_PHONE", tc.phone)
				t.Setenv("AUTH_TEST_ACCOUNT_CODE", tc.code)

				_, err := NewConfig()
				if err == nil {
					t.Fatal("expected an error, got nil")
				}
				if !strings.Contains(err.Error(), "AUTH_TEST_ACCOUNT_PHONE") {
					t.Errorf("error must name the variables, got %v", err)
				}
			})
		}
	})
}
