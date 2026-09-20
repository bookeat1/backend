// Package campaign serves GET /m/:slug — the QR-flyer resolver's mini-landing
// (spec marathon-remainder-plan-20260914.md, M1). Deliberately outside
// /api/v1: this is a page a phone's browser navigates to after a camera scan,
// not a JSON API call, so it renders plain server-side HTML
// (html/template, no frontend framework) rather than going through
// response.Envelope.
package campaign

import (
	"context"
	"html/template"
	"net/http"

	"github.com/gin-gonic/gin"

	"backend-core/internal/domain"
	"backend-core/internal/logging"
	"backend-core/internal/usecase/campaign"
)

// resolver is the slice of campaign.Facade this handler needs.
type resolver interface {
	Resolve(ctx context.Context, slug, userAgent string) (campaign.Outcome, *domain.CampaignLink, error)
}

// Handler serves the mini-landing.
type Handler struct {
	resolver resolver
	// webBaseURL is the origin the "Открыть на сайте" button points at
	// (`<webBaseURL>/?promo=<promotion_id>`). See bootstrap.AppConfig.WebBaseURL.
	webBaseURL string
}

// NewHandler builds the campaign resolver handler.
func NewHandler(r resolver, webBaseURL string) *Handler {
	return &Handler{resolver: r, webBaseURL: webBaseURL}
}

// RegisterRoutes mounts GET /m/:slug on r (the top-level engine, NOT the
// /api/v1 group — see the package doc).
func (h *Handler) RegisterRoutes(r gin.IRouter) {
	r.GET("/m/:slug", h.resolve)
}

func (h *Handler) resolve(c *gin.Context) {
	slug := c.Param("slug")
	outcome, link, err := h.resolver.Resolve(c.Request.Context(), slug, c.Request.UserAgent())
	if err != nil {
		logging.FromContext(c.Request.Context()).Error("campaign link resolve failed",
			"slug", slug, "error", err.Error())
		c.Data(http.StatusInternalServerError, "text/html; charset=utf-8", []byte(errorPageHTML))
		return
	}

	switch outcome {
	case campaign.OutcomeNotFound:
		c.Data(http.StatusNotFound, "text/html; charset=utf-8", []byte(notFoundPageHTML))
	case campaign.OutcomeDisabled:
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(endedPageHTML))
	case campaign.OutcomeActive:
		h.renderLanding(c, link)
	default:
		// Unreachable: Resolve only ever returns one of the three outcomes
		// above. Fails safe rather than serving an unbranded blank page.
		c.Data(http.StatusInternalServerError, "text/html; charset=utf-8", []byte(errorPageHTML))
	}
}

func (h *Handler) renderLanding(c *gin.Context, link *domain.CampaignLink) {
	data := landingData{
		TargetURL: link.TargetURL,
		WebURL:    h.webBaseURL + "/?promo=" + link.PromotionID.String(),
	}
	c.Writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	c.Writer.WriteHeader(http.StatusOK)
	if err := landingTemplate.Execute(c.Writer, data); err != nil {
		// Headers are already flushed at this point (WriteHeader above) — a
		// template error can only be logged, not turned into a different
		// status code.
		logging.FromContext(c.Request.Context()).Error("campaign landing render failed", "error", err.Error())
	}
}

type landingData struct {
	TargetURL string
	WebURL    string
}

// landingTemplate is the mini-landing shown for an active link. html/template
// auto-escapes TargetURL/WebURL for the href context, so a malformed
// target_url in the database can never inject markup — see the package doc
// for why this is plain html/template and not a frontend framework.
//
// No secrets, no keys, no analytics SDK: the scan itself is already counted
// server-side in campaign_link_hits (spec §6, open question 4).
var landingTemplate = template.Must(template.New("landing").Parse(`<!DOCTYPE html>
<html lang="ru">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>BookEat — Марафон Алматы 2026</title>
<style>
body{font-family:sans-serif;max-width:480px;margin:40px auto;padding:0 16px;text-align:center;color:#222}
h1{font-size:22px}
a.btn{display:block;margin:16px 0;padding:14px 20px;border-radius:10px;text-decoration:none;font-weight:600}
a.btn-primary{background:#111;color:#fff}
a.btn-secondary{background:#eee;color:#111}
</style>
</head>
<body>
<h1>Забронируй стол через BookEat, приходи, получи подарок от BookEat</h1>
<a class="btn btn-primary" href="{{.TargetURL}}">Установить / открыть приложение</a>
<a class="btn btn-secondary" href="{{.WebURL}}">Открыть на сайте</a>
</body>
</html>`))

const notFoundPageHTML = `<!DOCTYPE html>
<html lang="ru"><head><meta charset="utf-8"><title>BookEat</title></head>
<body><h1>Страница не найдена</h1></body></html>`

const endedPageHTML = `<!DOCTYPE html>
<html lang="ru"><head><meta charset="utf-8"><title>BookEat</title></head>
<body><h1>Акция завершена</h1></body></html>`

const errorPageHTML = `<!DOCTYPE html>
<html lang="ru"><head><meta charset="utf-8"><title>BookEat</title></head>
<body><h1>Что-то пошло не так, попробуйте ещё раз</h1></body></html>`
