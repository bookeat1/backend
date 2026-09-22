package kwaaka

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"backend-core/internal/domain"
)

// MenuSource implements domain.KwaakaMenuSource over the Table Booking API's
// GET /restaurants/{id}/menu.
type MenuSource struct{ client *Client }

// NewMenuSource builds the adapter.
func NewMenuSource(client *Client) *MenuSource { return &MenuSource{client: client} }

var _ domain.KwaakaMenuSource = (*MenuSource)(nil)

// FetchMenu calls GET /restaurants/{kwaakaRestaurantID}/menu?service=... and
// maps the Gourmet-shaped response into domain terms. See doc.go for what is
// verified and what is deliberately not mapped yet.
func (s *MenuSource) FetchMenu(ctx context.Context, kwaakaRestaurantID string) (domain.KwaakaMenu, error) {
	if strings.TrimSpace(kwaakaRestaurantID) == "" {
		return domain.KwaakaMenu{}, fmt.Errorf("%w: empty kwaaka restaurant id", domain.ErrValidation)
	}
	path := fmt.Sprintf("/restaurants/%s/menu?service=%s",
		url.PathEscape(kwaakaRestaurantID), url.QueryEscape(s.client.cfg.Service))
	raw, err := s.client.get(ctx, path)
	if err != nil {
		return domain.KwaakaMenu{}, fmt.Errorf("fetch kwaaka menu: %w", err)
	}
	var m rawMenu
	if err := json.Unmarshal(raw, &m); err != nil {
		return domain.KwaakaMenu{}, fmt.Errorf("%w: malformed kwaaka menu response: %v", domain.ErrUnavailable, err)
	}
	return mapMenu(m), nil
}

// --- raw Kwaaka JSON shapes (Menu / GourmetSection / GourmetProduct / Price /
// LanguageDescription from table-booking-swagger-v1.1.0.yaml). Field names
// match the wire format exactly; nothing here is exported or used outside
// this file — the translation into domain.KwaakaMenu below is the anti-
// corruption boundary.

type rawMenu struct {
	Sections []rawSection `json:"sections"`
	Products []rawProduct `json:"products"`
}

type rawSection struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	IsAvailable bool   `json:"is_available"`
	IsDeleted   bool   `json:"is_deleted"`
}

type rawProduct struct {
	ID          string       `json:"id"`
	Section     string       `json:"section"`
	Name        []rawLangVal `json:"name"`
	Description []rawLangVal `json:"description"`
	Images      []string     `json:"images"`
	IsAvailable bool         `json:"is_available"`
	Price       []rawPrice   `json:"price"`
}

type rawLangVal struct {
	Value string `json:"value"`
}

type rawPrice struct {
	Value float64 `json:"value"`
}

// mapMenu is the ACL: everything domain.KwaakaMenuProduct exposes is derived
// here, nothing raw leaks past this function.
func mapMenu(m rawMenu) domain.KwaakaMenu {
	sections := make(map[string]rawSection, len(m.Sections))
	for _, s := range m.Sections {
		sections[s.ID] = s
	}

	out := domain.KwaakaMenu{Products: make([]domain.KwaakaMenuProduct, 0, len(m.Products))}
	for _, p := range m.Products {
		id := strings.TrimSpace(p.ID)
		name := firstVal(p.Name)
		// A product with no id or no name cannot be correlated to a
		// menu_items row (id) or shown to a guest (name); skip rather than
		// upsert a blank dish.
		if id == "" || name == "" {
			continue
		}

		available := p.IsAvailable
		category := ""
		if sec, ok := sections[p.Section]; ok {
			category = sec.Name
			// A dish in a hidden/deleted section is exactly as unavailable to
			// a guest as one Kwaaka flagged directly — see doc.go.
			available = available && sec.IsAvailable && !sec.IsDeleted
		}
		if !hasPositivePrice(p.Price) {
			// A dish with no real price from the POS must never publish as
			// "free" — force it off the shelf regardless of whatever
			// is_available Kwaaka sent for the product/section. This
			// overrides, it never re-enables: a dish Kwaaka already marked
			// unavailable for its own reasons stays unavailable either way.
			available = false
		}

		out.Products = append(out.Products, domain.KwaakaMenuProduct{
			ExternalID:  id,
			Name:        name,
			Description: firstVal(p.Description),
			Price:       priceString(p.Price),
			ImageURL:    firstStr(p.Images),
			Category:    category,
			IsAvailable: available,
		})
	}
	return out
}

func firstVal(vs []rawLangVal) string {
	if len(vs) == 0 {
		return ""
	}
	return strings.TrimSpace(vs[0].Value)
}

func firstStr(vs []string) string {
	if len(vs) == 0 {
		return ""
	}
	return strings.TrimSpace(vs[0])
}

// priceString converts Kwaaka's first Price entry into the decimal-string
// format domain.ValidPrice expects, without ever letting the float leak
// beyond this one formatting call. A missing/negative price maps to "0.00"
// rather than being upserted as garbage. Note this value alone does NOT mean
// the dish is safe to publish — a "0.00" (or any non-positive) price forces
// IsAvailable to false in mapMenu (see hasPositivePrice) so it never reaches
// a guest as a free dish; "0.00" here only keeps the stored price column a
// well-formed decimal string.
func priceString(ps []rawPrice) string {
	if len(ps) == 0 || ps[0].Value < 0 {
		return "0.00"
	}
	return fmt.Sprintf("%.2f", ps[0].Value)
}

// hasPositivePrice reports whether Kwaaka actually sent a usable, strictly
// positive price for the product. Missing, empty, zero and negative all read
// as "no real price" — see mapMenu's use of this to force IsAvailable=false
// instead of ever letting a priceless dish publish as free.
func hasPositivePrice(ps []rawPrice) bool {
	return len(ps) > 0 && ps[0].Value > 0
}
