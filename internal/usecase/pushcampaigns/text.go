package pushcampaigns

import (
	"strings"
	"time"

	"backend-core/internal/domain"
)

// PushText is one rendered push (or preview tile): a title and a body, both
// ready to send — no further templating happens downstream.
type PushText struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// previewLangs is the fixed set the estimate preview always renders,
// regardless of who ends up receiving the campaign (criterion 3) — ru/kk/en,
// the platform's three supported guest-facing languages for this feature.
var previewLangs = []string{domain.LocaleRU, domain.LocaleKK, domain.LocaleEN}

// header text per (kind, is-platform, lang). Not data-driven: four fixed
// sentences translated once, the same discipline as any other short UI
// copy baked into a template rather than fetched from a table.
var headerTemplates = map[string]map[string]string{
	"event_venue":    {"ru": "Новое событие в «%s»", "kk": "«%s» мекемесінде жаңа іс-шара", "en": "New event at %s"},
	"event_platform": {"ru": "Афиша BookEat", "kk": "BookEat афишасы", "en": "BookEat Guide"},
	"promo_venue":    {"ru": "Акция в «%s»", "kk": "«%s» мекемесінде акция", "en": "Offer at %s"},
	"promo_platform": {"ru": "Акция BookEat", "kk": "BookEat акциясы", "en": "BookEat Offer"},
}

// untilWord is the connector before a promo's end date ("до 30.09").
var untilWord = map[string]string{"ru": "до", "kk": "дейін", "en": "until"}

// maxBodyLen is criterion 20's ceiling. A venue name long enough to blow
// through it is vanishingly unlikely at BookEat's catalog, but the truncation
// keeps the guarantee absolute rather than "usually true".
const maxBodyLen = 120

// renderText builds one language's push text for a subject. lang falls back
// to Russian for anything not in previewLangs or not present in TitleI18n —
// "без перевода — русский" (criterion 20).
func renderText(s domain.PushCampaignSubject, lang string, loc *time.Location) PushText {
	key := string(s.Kind)
	if s.IsPlatform() {
		key += "_platform"
	} else {
		key += "_venue"
	}
	tmpl, ok := headerTemplates[key][lang]
	if !ok {
		tmpl = headerTemplates[key][domain.LocaleRU]
	}
	title := tmpl
	if strings.Contains(tmpl, "%s") {
		title = strings.Replace(tmpl, "%s", "«"+s.VenueName+"»", 1)
	}

	body := s.TitleI18n.Resolve(lang, s.Title)
	body = body + " · " + dateFragment(s, lang, loc)
	if len(body) > maxBodyLen {
		body = body[:maxBodyLen]
	}
	return PushText{Title: title, Body: body}
}

// dateFragment is the "· 26.09 в 19:00" (event) or "· до 30.09" (promo) tail,
// rendered in loc (BOOKING_TIMEZONE_FALLBACK) so a campaign fanned out across
// one city reads the same local time for everyone, matching the convention
// buildGuestMessage already uses for booking pushes.
func dateFragment(s domain.PushCampaignSubject, lang string, loc *time.Location) string {
	if s.Kind == domain.PushCampaignKindPromo {
		word := untilWord[lang]
		if word == "" {
			word = untilWord[domain.LocaleRU]
		}
		return word + " " + s.EndsAt.In(loc).Format("02.01")
	}
	return s.StartsAt.In(loc).Format("02.01 в 15:04")
}

// renderPreview builds the fixed ru/kk/en preview the estimate response
// always carries (criterion 3), independent of any single guest's language.
func renderPreview(s domain.PushCampaignSubject, loc *time.Location) map[string]PushText {
	out := make(map[string]PushText, len(previewLangs))
	for _, lang := range previewLangs {
		out[lang] = renderText(s, lang, loc)
	}
	return out
}
