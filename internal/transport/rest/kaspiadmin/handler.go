// Package kaspiadmin exposes the platform's read-only view of our Kaspi
// service's company registry.
//
// It exists for one screen: «Приём оплаты» in the admin panel, where a
// superadmin points a venue's money at one of the companies inside the Kaspi
// service (PUT /admin/restaurants/:id/payment-settings/acquirer-account, whose
// account_ref is exactly the id listed here). Before this, that id was typed
// by hand from another panel — the kind of copy-paste that credits the wrong
// till.
//
// SUPERADMIN ONLY. The list names every merchant on the platform's payment
// service, which is a platform fact and none of a venue's business; and it is
// the input to the one setting that decides whose account a guest's money
// lands in. The route is mounted on the RequireRole(domain.RoleAdmin) group AND
// re-checks the role here, the same belt-and-braces every other platform-only
// handler in this tree uses.
//
// READ-ONLY, and it must stay that way: Kaspi has no sandbox, so a write here
// would move real money on its first call.
package kaspiadmin

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"backend-core/internal/domain"
	kaspigw "backend-core/internal/infrastructure/payment/kaspi"
	"backend-core/internal/transport/rest/middleware"
	"backend-core/internal/transport/rest/response"
)

// CompanyDirectory is the slice of the Kaspi service this handler needs.
// Implemented by *kaspi.Directory.
type CompanyDirectory interface {
	ListCompanies(ctx context.Context) ([]kaspigw.Company, error)
}

// Handler serves the Kaspi company registry.
type Handler struct {
	directory CompanyDirectory
	log       *slog.Logger
}

// NewHandler wires the directory. directory may be nil — a deployment with no
// Kaspi service configured answers 503 rather than failing to boot, exactly
// like the payment adapter, which is skipped when its env is absent.
func NewHandler(directory CompanyDirectory, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Handler{directory: directory, log: log}
}

// RegisterAdminGlobal mounts the read. MUST be mounted on a
// RequireRole(domain.RoleAdmin) group; the handler re-checks anyway.
func (h *Handler) RegisterAdminGlobal(rg *gin.RouterGroup) {
	rg.GET("/admin/kaspi/companies", h.listCompanies)
}

// companyResponse is the panel's view of one company on the payment service.
// Only what the picker renders: an address, a name, and whether it can take
// money right now.
type companyResponse struct {
	// ID is what to send back as acquirer-account `account_ref`.
	ID      string `json:"id"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	OrgName string `json:"org_name,omitempty"`
	// HasActiveSession is the difference between a company that can take money
	// and one that only looks like it can: Kaspi evicts a cashier session
	// whenever the same device registers again, and the company stays `active`
	// with no way to create a payment link until someone re-authenticates.
	HasActiveSession bool       `json:"has_active_session"`
	ActiveCashiers   int        `json:"active_cashiers"`
	LastSessionOKAt  *time.Time `json:"last_session_ok_at,omitempty"`
}

// listCompanies answers with every company on the Kaspi service.
//
// @Summary     List the Kaspi service's companies (superadmin)
// @Description Read-only registry of the companies on our Kaspi payment
// @Description service. Its `id` is the `account_ref` of
// @Description PUT /admin/restaurants/{id}/payment-settings/acquirer-account.
// @Tags        admin
// @Produce     json
// @Success     200 {object} response.Envelope
// @Failure     401 {object} response.Envelope
// @Failure     403 {object} response.Envelope
// @Failure     503 {object} response.Envelope
// @Security    BearerAuth
// @Router      /admin/kaspi/companies [get]
func (h *Handler) listCompanies(c *gin.Context) {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	if au.Role != string(domain.RoleAdmin) {
		response.Error(c.Writer, http.StatusForbidden, "forbidden")
		return
	}
	if h.directory == nil {
		response.Error(c.Writer, http.StatusServiceUnavailable, "kaspi service is not configured")
		return
	}

	companies, err := h.directory.ListCompanies(c.Request.Context())
	if err != nil {
		// The panel gets the generic 503 from HandleError; the reason (which
		// names no credential — see kaspi.Directory) is logged for whoever has
		// to go and look at the service.
		h.log.Warn("kaspi company directory unavailable", slog.String("error", err.Error()))
		response.HandleError(c.Writer, err)
		return
	}

	out := make([]companyResponse, 0, len(companies))
	for _, company := range companies {
		out = append(out, companyResponse{
			ID:               company.ID,
			Name:             company.Name,
			Status:           company.Status,
			OrgName:          company.OrgName,
			HasActiveSession: company.HasActiveSession,
			ActiveCashiers:   company.ActiveCashiers,
			LastSessionOKAt:  company.LastSessionOKAt,
		})
	}
	response.OK(c.Writer, out)
}
