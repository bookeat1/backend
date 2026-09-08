package staticmap

import (
	"errors"
	"testing"

	"backend-core/internal/domain"
)

func TestParseParamsDefaults(t *testing.T) {
	p, err := ParseParams("", "", "")
	if err != nil {
		t.Fatalf("ParseParams: %v", err)
	}
	want := Params{Width: 480, Height: 270, Scale: 1, Zoom: 16}
	if p != want {
		t.Errorf("defaults = %+v, want %+v", p, want)
	}
}

func TestParseParamsAcceptsWhitelist(t *testing.T) {
	tests := []struct {
		size, scale, zoom string
		want              Params
	}{
		{"card", "1", "14", Params{320, 180, 1, 14}},
		{"detail", "2", "16", Params{480, 270, 2, 16}},
		{"wide", "2", "18", Params{640, 360, 2, 18}},
		{" wide ", " 1 ", " 15 ", Params{640, 360, 1, 15}}, // surrounding spaces are tolerated
	}
	for _, tc := range tests {
		got, err := ParseParams(tc.size, tc.scale, tc.zoom)
		if err != nil {
			t.Fatalf("ParseParams(%q,%q,%q): %v", tc.size, tc.scale, tc.zoom, err)
		}
		if got != tc.want {
			t.Errorf("ParseParams(%q,%q,%q) = %+v, want %+v", tc.size, tc.scale, tc.zoom, got, tc.want)
		}
	}
}

// Out-of-whitelist input is REJECTED, never clamped: silently serving a
// different picture than asked for hides client bugs, and a free-form size is
// exactly how this endpoint would become a way to bill us for huge renders.
func TestParseParamsRejectsAnythingOutsideTheWhitelist(t *testing.T) {
	tests := []struct {
		name              string
		size, scale, zoom string
	}{
		{"free-form size", "1280x1280", "", ""},
		{"unknown size name", "huge", "", ""},
		{"size injection attempt", "detail&pt=0,0", "", ""},
		{"scale 3", "", "3", ""},
		{"scale 0", "", "0", ""},
		{"scale negative", "", "-1", ""},
		{"scale not a number", "", "x2", ""},
		{"zoom too low", "", "", "1"},
		{"zoom too high", "", "", "19"},
		{"zoom huge", "", "", "999999"},
		{"zoom not a number", "", "", "sixteen"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseParams(tc.size, tc.scale, tc.zoom)
			if !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("error = %v, want domain.ErrValidation (422)", err)
			}
		})
	}
}

// Every whitelisted size must stay inside 2GIS's documented 120–1280 width /
// 90–1280 height limits, at BOTH scales — @2x doubles the rendered pixels.
func TestSizeWhitelistFitsProviderLimits(t *testing.T) {
	for _, s := range sizes {
		for _, scale := range []int{1, 2} {
			w, h := s.width*scale, s.height*scale
			if w < 120 || w > 1280 {
				t.Errorf("size %q at @%dx: width %d outside 2GIS's 120..1280", s.name, scale, w)
			}
			if h < 90 || h > 1280 {
				t.Errorf("size %q at @%dx: height %d outside 2GIS's 90..1280", s.name, scale, h)
			}
		}
	}
}

func TestZoomWhitelistFitsProviderLimits(t *testing.T) {
	for _, z := range zooms {
		if z < 1 || z > 18 {
			t.Errorf("zoom %d outside 2GIS's documented 1..18", z)
		}
	}
}
