package main

import (
	"strings"
	"testing"

	"backend-core/internal/domain"
)

func TestNormalizeName(t *testing.T) {
	cases := map[string]string{
		"  Caesar Salad ":  "caesar salad",
		"CAESAR SALAD":     "caesar salad",
		"caesar salad":     "caesar salad",
		"Цезарь с курицей": "цезарь с курицей",
	}
	for in, want := range cases {
		if got := NormalizeName(in); got != want {
			t.Errorf("NormalizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseMenuFile(t *testing.T) {
	data := []byte(`[
		{"section":"Salads","name":"Caesar Salad","price":4500,"unit":"250 g"},
		{"section":"Drinks","name":"Latte","name_en":"Latte EN","price":1200,"description":"hot"}
	]`)
	items, err := ParseMenuFile(data)
	if err != nil {
		t.Fatalf("ParseMenuFile: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
	if items[0].Name != "Caesar Salad" || *items[0].Price != 4500 {
		t.Errorf("item 0 = %+v", items[0])
	}
	if items[1].NameEN == nil || *items[1].NameEN != "Latte EN" {
		t.Errorf("item 1 NameEN = %v", items[1].NameEN)
	}
}

func TestParseMenuFileRejectsInvalidJSON(t *testing.T) {
	_, err := ParseMenuFile([]byte(`not json`))
	if err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
}

func TestToDomainFieldsRejectsMissingOrNegativePrice(t *testing.T) {
	var m domain.MenuItem

	if err := (ParsedItem{Name: "No price"}).ToDomainFields(&m); err == nil {
		t.Error("expected error for nil price")
	}
	neg := int64(-1)
	if err := (ParsedItem{Name: "Negative", Price: &neg}).ToDomainFields(&m); err == nil {
		t.Error("expected error for negative price")
	}
}

func TestToDomainFieldsMapsAllFields(t *testing.T) {
	price := int64(4500)
	unit := " 250 g "
	desc := " tasty "
	nameEN := " Caesar Salad "
	img := "https://example.com/img.jpg"
	p := ParsedItem{
		Section:     " Salads ",
		Name:        "  Caesar Salad ",
		NameEN:      &nameEN,
		Description: &desc,
		Price:       &price,
		Unit:        &unit,
		ImageURL:    &img,
	}
	var m domain.MenuItem
	if err := p.ToDomainFields(&m); err != nil {
		t.Fatalf("ToDomainFields: %v", err)
	}
	if m.Name != "Caesar Salad" {
		t.Errorf("Name = %q", m.Name)
	}
	if m.Price != "4500.00" {
		t.Errorf("Price = %q", m.Price)
	}
	if m.Description != "tasty" {
		t.Errorf("Description = %q", m.Description)
	}
	if m.Category == nil || *m.Category != "Salads" {
		t.Errorf("Category = %v", m.Category)
	}
	if m.PortionSize == nil || *m.PortionSize != "250 g" {
		t.Errorf("PortionSize = %v", m.PortionSize)
	}
	if m.ImageURL == nil || *m.ImageURL != img {
		t.Errorf("ImageURL = %v", m.ImageURL)
	}
	if m.NameI18n[domain.LocaleEN] != "Caesar Salad" {
		t.Errorf("NameI18n = %v", m.NameI18n)
	}
}

func TestToDomainFieldsPrefersImageURLOverSourceImage(t *testing.T) {
	price := int64(100)
	imgURL := "https://cdn/a.jpg"
	src := "https://cdn/b.jpg"
	p := ParsedItem{Name: "x", Price: &price, ImageURL: &imgURL, SourceImage: &src}
	var m domain.MenuItem
	if err := p.ToDomainFields(&m); err != nil {
		t.Fatalf("ToDomainFields: %v", err)
	}
	if m.ImageURL == nil || *m.ImageURL != imgURL {
		t.Errorf("ImageURL = %v, want %q", m.ImageURL, imgURL)
	}
}

func TestToDomainFieldsFallsBackToSourceImage(t *testing.T) {
	price := int64(100)
	src := "https://cdn/b.jpg"
	p := ParsedItem{Name: "x", Price: &price, SourceImage: &src}
	var m domain.MenuItem
	if err := p.ToDomainFields(&m); err != nil {
		t.Fatalf("ToDomainFields: %v", err)
	}
	if m.ImageURL == nil || *m.ImageURL != src {
		t.Errorf("ImageURL = %v, want %q", m.ImageURL, src)
	}
}

func TestToDomainFieldsClearsCategoryAndPortionWhenBlank(t *testing.T) {
	price := int64(100)
	p := ParsedItem{Name: "x", Price: &price, Section: "   ", Unit: strPtr("  ")}
	var m domain.MenuItem
	m.Category = strPtr("stale")
	m.PortionSize = strPtr("stale")
	if err := p.ToDomainFields(&m); err != nil {
		t.Fatalf("ToDomainFields: %v", err)
	}
	if m.Category != nil {
		t.Errorf("Category = %v, want nil", m.Category)
	}
	if m.PortionSize != nil {
		t.Errorf("PortionSize = %v, want nil", m.PortionSize)
	}
}

func strPtr(s string) *string { return &s }

func TestPriceStringHasTwoDecimals(t *testing.T) {
	price := int64(999)
	p := ParsedItem{Name: "x", Price: &price}
	var m domain.MenuItem
	if err := p.ToDomainFields(&m); err != nil {
		t.Fatalf("ToDomainFields: %v", err)
	}
	if !strings.HasSuffix(m.Price, ".00") {
		t.Errorf("Price = %q, want a .00 suffix", m.Price)
	}
	if !domain.ValidPrice(m.Price) {
		t.Errorf("Price %q is not a valid domain price string", m.Price)
	}
}
