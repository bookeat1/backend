// Package reqlocale resolves the caller's requested response language from an
// HTTP request, shared by every REST handler that serves domain.I18n content
// (restaurants today, favorites reuses the same restaurant fields).
package reqlocale

import (
	"strings"

	"github.com/gin-gonic/gin"

	"backend-core/internal/domain"
)

// Resolve determines which language the caller explicitly asked for, via (in
// priority order) the `lang` query parameter or the Accept-Language header.
// It returns "" when the caller gave no signal at all, in which case callers
// must leave their response's base scalar fields (name, description, ...)
// untouched — this is what keeps the JSON shape identical for every existing
// client that never asks for a language.
//
// Tags travel through domain.NormalizeLocale, so a region subtag ("kk-KZ") and
// the historical alias "kz" — which old store builds send and which is what the
// imported menu rows were labelled with — both resolve to "kk".
//
// A caller that DOES ask for something, but names an unsupported/unparseable
// language, still gets an explicit "ru" back rather than being silently
// treated as "asked for nothing" — ru is always a safe answer since it is the
// same text the base columns already hold.
func Resolve(c *gin.Context) string {
	if v := strings.TrimSpace(c.Query("lang")); v != "" {
		if l := domain.NormalizeLocale(v); l != "" {
			return l
		}
		return domain.LocaleRU
	}
	h := strings.TrimSpace(c.GetHeader("Accept-Language"))
	if h == "" {
		return ""
	}
	for _, part := range strings.Split(h, ",") {
		tag := strings.SplitN(strings.TrimSpace(part), ";", 2)[0]
		if l := domain.NormalizeLocale(tag); l != "" {
			return l
		}
	}
	return domain.LocaleRU
}
