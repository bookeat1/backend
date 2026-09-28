package tiptoppay

import (
	"context"
	"net/http"
	"testing"
)

func TestAuthorizeReturnURLSubstitution(t *testing.T) {
	const fallback = "https://api.example.kz"
	cases := []struct {
		name, returnURL, fallback, want string
	}{
		{"deep link -> fallback", "bookeat://booking/abc/payment", fallback, fallback},
		{"empty -> fallback", "", fallback, fallback},
		{"https unchanged", "https://bookeat.kz/pay/return", fallback, "https://bookeat.kz/pay/return"},
		{"deep link, no fallback -> omitted", "bookeat://booking/abc/payment", "", ""},
		{"garbage fallback -> omitted", "bookeat://x", "not a url", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAcquirer(t, func(_ string, _ int, _ map[string]any, w http.ResponseWriter) {
				ok(w, `{"Id":"o1","Currency":"KZT","Url":"https://orders.tiptoppay.kz/d/o1","Status":"Created"}`)
			})
			g := f.gateway(t, nil)
			g.cfg.ReturnFallbackURL = tc.fallback

			req := authorizeRequest()
			req.ReturnURL = tc.returnURL
			if _, err := g.Authorize(context.Background(), req); err != nil {
				t.Fatalf("Authorize: %v", err)
			}
			body := f.seen()[0].Body
			for _, k := range []string{"SuccessRedirectUrl", "FailRedirectUrl"} {
				got, _ := body[k].(string)
				if got != tc.want {
					t.Errorf("%s = %q, want %q", k, got, tc.want)
				}
			}
		})
	}
}
