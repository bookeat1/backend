package media

import (
	"encoding/base64"
	"testing"
)

// A real WebP file, 900x600, flat colour so it stays tiny. Generated with
// Pillow; kept as base64 because Go cannot ENCODE WebP, so this fixture
// cannot be built at test time by the package it tests.
//
// It guards a decoder that was genuinely missing: the first dry run over the
// live bucket reported 6 "unsupported" results, which was the 3 .webp
// originals times the 2 sizes. Delete the golang.org/x/image/webp import and
// this test goes red.
const webpFixtureB64 = "" +
	"UklGRogEAABXRUJQVlA4IHwEAAAweACdASqEA1gCPrVaqlCnJSOioAgA4BaJaW7hd2Eaw9iAAAKzdrxcnIe+2TkPfbJy" +
	"Hvtk5D32ych77ZOQ+1V9snIe+2TkPfbJyHvtk5D32ych77ZOQ99snIe+2TkPfbJyHvtk5D32ych77ZOQ99snIe+2TkPf" +
	"bJyHvtk5D32ych77ZOQ99snIe+2TkPfbJyHvtk5D32ych77ZOQ99snIe+2TkPfbJyHvtk5D32ych77ZOQ99snIe+2TkP" +
	"fbJyHvtk5D32ych77ZOQ99snIe+2TkPfbJyHvtk5D32ych77ZOQ99snIe+2TkPfbJyHvtk5D32ych77ZOQ99snIe+2Tk" +
	"PfbJyHvtk5D32ych77ZOQ99snIe+2TkPfbJyHvtk5D32ych77ZOQ99snIe+2TkPfbJyHvtk5D32ych77ZOQ99snIe+2T" +
	"kPfbJyHvtk5D32ych77ZOQ99snIe+2TkPfbJyHvtk5D32ych77ZOQ99snIe+2TkPfbJyHvtk5D32ych77ZOQ99snIe+2" +
	"TkPfbJyHvtk5D32ych77ZOQ99snIe+2TkPfbJyHvtk5D32ych77ZOQ99snIe+2TkPfbJyHvtk5D32ych77ZOQ99snIe+" +
	"2TkPfbJyHvtk5D32ych77ZOQ99snIe+2TkPfbJyHvtk5D32ych77ZOQ99snIe+2TkPfbJyHvtk5D32ych77ZOQ99snIe" +
	"+2TkPfbJyHvtk5D32ych77ZOQ99snIe+2TkPfbJyHvtk5D32ych77ZOQ99snIe+2TkPfbJyHvtk5D32ych77ZOQ99snI" +
	"e+2TkPfbJyHvtk5D32ych77ZOQ99snIe+2TkPfbJyHvtk5D32ych77ZOQ99snIe+2TkPfbJyHvtk5D32ych77ZOQ99sn" +
	"Ie+2TkPfbJyHvtk5D32ych77ZOQ99snIe+2TkPfbJyHvtk5D32ych77ZOQ99snIe+2TkPfbJyHvtk5D32ych77ZOQ99s" +
	"nIe+2TkPfbJyHvtk5D32ych77ZOQ99snIe+2TkPfbJyHvtk5D32ych77ZOQ99snIe+2TkPfbJyHvtk5D32ych77ZOQ99" +
	"snIe+2TkPfbJyHvtk5D32ych77ZOQ99snIe+2TkPfbJyHvtk5D32ych77ZOQ99snIe+2TkPfbJyHvtk5D32ych77ZOQ9" +
	"9snIe+2TkPfbJyHvtk5D32ych77ZOQ99snIe+2TkPfbJyHvtk5D32ych77ZOQ99snIe+2TkPfbJyHvtk5D32ych77ZOQ" +
	"99snIe+2TkPfbJyHvtk5D32ych77ZOFAAP7/bhv//O3pt6bQH//WrmVzKGmG2yLPQwd9Bd8CAJ5o4AKDQAgHhoAQDw0A" +
	"IB4aAEA8NACAeGgBAPDQAgHhoAQDw0AIB4aAEA8NACAeGgBAPDQAgHhoAQDw0AIB4aAEA8NACAeGgBAPDQAgHhoAQDw0" +
	"AIB4aAEA8NACAeGgBAPDQAgHhoAQDw0AIB4aAEA8NACAeGgBAPDQAgHhoAQDw0AIB4aAEA8NACAeGgBAAAAAAA=="

func TestRenderDecodesWebPSources(t *testing.T) {
	src, err := base64.StdEncoding.DecodeString(webpFixtureB64)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}

	got, err := Render(src, WidthSmall)
	if err != nil {
		t.Fatalf("Render of a WebP source: %v", err)
	}
	if got.Width != WidthSmall {
		t.Fatalf("width = %d, want %d", got.Width, WidthSmall)
	}
	// 900x600 is 3:2 -> 640 wide is 427 high.
	if got.Height != 427 {
		t.Fatalf("height = %d, want 427", got.Height)
	}
	if got.ContentType != "image/jpeg" {
		t.Fatalf("content type = %q, want image/jpeg", got.ContentType)
	}
}
