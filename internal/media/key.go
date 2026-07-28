// Package media turns a stored photo original into the small, display-sized
// derivatives the apps actually need, and names those derivatives so that any
// consumer can address one without asking the database anything.
//
// WHY THIS EXISTS. Every photo in the catalog is served to phones as the byte
// stream somebody uploaded. Measured against the live bucket on 2026-07-27:
// 772 objects, 477.7 MB; the 55 venue photos alone are 81.6 MB, the largest
// single cover is 7.79 MB. A venue card renders that cover 148pt tall. So the
// app spends multiple megabytes of a guest's mobile data to fill a box the
// size of a business card, on exactly the connection where that hurts most.
//
// Caching is NOT the problem and this package does not try to fix it: R2
// already answers with `Cache-Control: public, max-age=31536000, immutable`
// and an ETag. The problem is purely that the bytes are too many.
//
// The fix is deliberately the boring one: write a couple of resized copies
// once, next to the original, and let the client ask for the small one. The
// alternative — Cloudflare Image Resizing over the same bucket — is a
// recurring bill for something that is done once and then never changes,
// because an uploaded photo is immutable by construction (its key contains a
// timestamp and a random suffix; editing a photo produces a new key, never new
// bytes under an old one).
package media

import (
	"path"
	"strings"
)

// DerivedPrefix is the single top-level prefix under which every generated
// object lives. Nothing else in the bucket ever writes here.
//
// This is a safety property, not a cosmetic one. Originals live under
// `restaurants/`, `menu/` and `events/`; derivatives live under `derived/`
// and nowhere else. That makes the dangerous operations structurally
// impossible rather than merely unlikely: a generator can only ever create
// keys beginning with `derived/`, so no bug in it can overwrite an original,
// and a future "throw away all derivatives and regenerate" is a delete of one
// prefix that provably contains no source material. There is no backup of the
// originals — this separation is what stands in for one.
const DerivedPrefix = "derived/"

// Widths are the two derivative sizes, in pixels of the long edge... no: of
// the WIDTH. Height follows the source aspect ratio, because the app crops
// with `contentFit: "cover"` at several different aspect ratios and a
// server-side crop would have to guess which one.
//
// WHY TWO, AND WHY THESE TWO. Taken from what the mobile app actually renders
// (apps/mobile/src, read on 2026-07-27) multiplied by 3, the device pixel
// ratio of the phones in question — a 2x phone gets a slightly sharper image
// than it needs, which is the correct direction to err:
//
//	tile / small (WidthSmall = 640)
//	  PopularRestaurantCard thumb   76pt  → 228px
//	  PromoBannerStrip            104pt  → 312px
//	  MenuItemCard / DishCard     180pt  → 540px
//	640 covers the largest of these (540px) with room to spare.
//
//	full-bleed / large (WidthLarge = 1280)
//	  RestaurantCard cover     full width × 148pt
//	  HeroCarousel             full width
//	  PhotoGrid gallery row    full width × 375pt
//	The widest phone in the fleet is ~430pt logical → 1290px at 3x. 1280 is
//	that, rounded to something a human can read; the 10px shortfall is not
//	visible on a photo that is being cropped anyway.
//
// Three sizes were considered and rejected. A middle 960 would sit between two
// values that are already only one doubling apart, would save a few KB on one
// screen, and would cost a third of the storage and a third of the backfill
// for it. Two sizes is the point where the next one stops paying for itself.
const (
	WidthSmall = 640
	WidthLarge = 1280
)

// Widths lists the sizes to generate, small first. Ordering matters to the
// backfill only in that it makes its log readable.
var Widths = []int{WidthSmall, WidthLarge}

// Ext is the extension — and therefore the format — of every derivative.
//
// JPEG, not WebP, even though three of the objects in the bucket happen to be
// .webp already. Reasons, in order of weight:
//
//  1. The sources are not WebP. Measured: 714 .jpg + 8 .jpeg + 47 .png + 3
//     .webp. The premise that "everything is already WebP" does not survive a
//     listing of the bucket.
//  2. Go's standard library encodes JPEG and cannot encode WebP
//     (golang.org/x/image/webp is a decoder only). Shipping WebP output means
//     adding either a cgo binding or a third-party pure-Go encoder to a
//     dependency list that is currently very short, for a one-off batch job.
//  3. At these sizes the win would be small in absolute terms. WebP typically
//     saves 25-30% over JPEG at equal quality; 30% of a 45 KB thumbnail is
//     13 KB, against the 1.2 MB we are removing. The interesting order of
//     magnitude is already captured.
//
// If WebP is wanted later it costs nothing to add: it is a new width-like
// constant and a second `derived/` sub-prefix, with the same fallback chain
// on the client. Nothing here has to be undone.
const Ext = ".jpg"

// DerivedKey returns the object key of the `width`-pixel derivative of
// `originalKey`.
//
//	restaurants/<uuid>/1751414713631-va1ag209cl.jpg
//	  → derived/w640/restaurants/<uuid>/1751414713631-va1ag209cl.jpg.jpg
//
// The original key is kept WHOLE, extension included, and the derivative's own
// extension is appended rather than substituted. It looks odd and it is
// deliberate: `a/b.jpg` and `a/b.png` are two different objects that may both
// exist, and replacing the extension would map them onto the same derivative
// key — one venue's photo silently served in place of another's. Appending
// cannot collide, because the map from original key to derived key is
// injective by construction.
//
// WHY DERIVE THE NAME INSTEAD OF STORING IT. The obvious alternative is a
// column (or a JSON blob) holding the derivative URLs next to each row. That
// would mean: a migration; a backfill of the rows as well as the objects; the
// OLD live app — which still writes image_url today — taught to populate it
// too, or every row it inserts is silently sizeless; and a permanent
// consistency question ("the column says there is a 640 but is there?"). A
// pure function has none of those. The client already holds the original URL;
// deriving is free, needs no round trip, and adding a third size later is a
// deploy rather than a data migration.
//
// The price of that choice is that the URL can point at an object that does
// not exist — a photo uploaded after the last backfill run. That is not
// hypothetical and must not be papered over: the consumer is REQUIRED to fall
// back to the original on a failed load, so a missing derivative costs one
// wasted request and the guest still sees their photo. See
// apps/mobile/src/lib/photo-source.ts for the other half of this contract.
//
// Returns "" for a key it will not name: an empty key, or one that is already
// a derivative. The second case is what stops a backfill run that is pointed
// at the whole bucket from generating derivatives of derivatives, growing the
// bucket geometrically one run at a time.
func DerivedKey(originalKey string, width int) string {
	key := strings.TrimSpace(originalKey)
	if key == "" || width <= 0 {
		return ""
	}
	if IsDerived(key) {
		return ""
	}
	return DerivedPrefix + widthSegment(width) + "/" + key + Ext
}

// IsDerived reports whether key names a generated object rather than an
// original. It is the guard every writer and every lister runs.
func IsDerived(key string) bool {
	return strings.HasPrefix(strings.TrimSpace(key), DerivedPrefix)
}

// widthSegment is the path segment naming a size: 640 → "w640".
func widthSegment(width int) string {
	return "w" + itoa(width)
}

// itoa avoids pulling strconv in for one call site and keeps the formatting of
// a width in exactly one place.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// DerivedURL is the URL form of DerivedKey, for callers that hold a public URL
// rather than a key. It exists so that the "how is a derivative addressed"
// rule lives in one file even though two different kinds of caller need it.
//
// base must be the public bucket root, with or without a trailing slash.
// Returns "" when DerivedKey would.
func DerivedURL(base, originalKey string, width int) string {
	dk := DerivedKey(originalKey, width)
	if dk == "" {
		return ""
	}
	return strings.TrimRight(base, "/") + "/" + dk
}

// KeyFromURL extracts the object key from a public bucket URL, or "" when the
// URL does not belong to that bucket. Used by tools that are handed the
// image_url column rather than a key.
func KeyFromURL(base, rawURL string) string {
	b := strings.TrimRight(strings.TrimSpace(base), "/") + "/"
	u := strings.TrimSpace(rawURL)
	if !strings.HasPrefix(u, b) {
		return ""
	}
	key := strings.TrimPrefix(u, b)
	// Defensive: a query string or fragment is not part of the key. Signed
	// Supabase URLs arrive with one and must not become part of a bucket path.
	if i := strings.IndexAny(key, "?#"); i >= 0 {
		key = key[:i]
	}
	return path.Clean("/" + key)[1:]
}
