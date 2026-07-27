package media

import "testing"

const testBase = "https://pub-41b6f06fc8e74b6e959cdd6def081e22.r2.dev/"

func TestDerivedKeyIsDeterministicAndNamespaced(t *testing.T) {
	const orig = "restaurants/d2f0e053-61b9-407a-8816-ceb370d65d22/1751414713631-va1ag209cl.jpg"

	got := DerivedKey(orig, WidthSmall)
	want := "derived/w640/restaurants/d2f0e053-61b9-407a-8816-ceb370d65d22/1751414713631-va1ag209cl.jpg.jpg"
	if got != want {
		t.Fatalf("DerivedKey small = %q, want %q", got, want)
	}
	if again := DerivedKey(orig, WidthSmall); again != got {
		t.Fatalf("DerivedKey is not deterministic: %q then %q", got, again)
	}
	if large := DerivedKey(orig, WidthLarge); large != "derived/w1280/"+orig+".jpg" {
		t.Fatalf("DerivedKey large = %q", large)
	}
}

// The whole safety story of this feature rests on derivatives being unable to
// address an original. If this ever fails, the backfill can overwrite the only
// copy of a photo.
func TestDerivedKeyAlwaysLandsUnderDerivedPrefix(t *testing.T) {
	for _, orig := range []string{
		"restaurants/x/a.jpg",
		"menu/y/b.png",
		"events/z/c.webp",
		"weird key with spaces.JPG",
	} {
		for _, w := range Widths {
			k := DerivedKey(orig, w)
			if k == "" {
				t.Fatalf("DerivedKey(%q,%d) unexpectedly empty", orig, w)
			}
			if !IsDerived(k) {
				t.Fatalf("DerivedKey(%q,%d) = %q, which is not under %q", orig, w, k, DerivedPrefix)
			}
			if k == orig {
				t.Fatalf("DerivedKey(%q,%d) returned the original key", orig, w)
			}
		}
	}
}

// Replacing the extension instead of appending it would map these two distinct
// objects onto one derivative, serving one venue's photo for another's.
func TestDerivedKeyDoesNotCollideAcrossSourceFormats(t *testing.T) {
	a := DerivedKey("restaurants/u/photo.jpg", WidthSmall)
	b := DerivedKey("restaurants/u/photo.png", WidthSmall)
	if a == b {
		t.Fatalf("distinct originals collided onto %q", a)
	}
}

// A backfill pointed at the whole bucket lists derivatives too. If it can name
// a derivative of a derivative, every run multiplies the object count.
func TestDerivedKeyRefusesToDeriveADerivative(t *testing.T) {
	once := DerivedKey("restaurants/u/photo.jpg", WidthSmall)
	if twice := DerivedKey(once, WidthSmall); twice != "" {
		t.Fatalf("derived a derivative: %q -> %q", once, twice)
	}
	if twice := DerivedKey(once, WidthLarge); twice != "" {
		t.Fatalf("derived a derivative across sizes: %q -> %q", once, twice)
	}
}

func TestDerivedKeyRejectsJunk(t *testing.T) {
	for _, tc := range []struct {
		key   string
		width int
	}{
		{"", WidthSmall},
		{"   ", WidthSmall},
		{"restaurants/u/photo.jpg", 0},
		{"restaurants/u/photo.jpg", -640},
	} {
		if got := DerivedKey(tc.key, tc.width); got != "" {
			t.Fatalf("DerivedKey(%q,%d) = %q, want empty", tc.key, tc.width, got)
		}
	}
}

func TestDerivedURLMatchesDerivedKey(t *testing.T) {
	const orig = "restaurants/u/photo.jpg"
	want := testBase + DerivedKey(orig, WidthSmall)
	if got := DerivedURL(testBase, orig, WidthSmall); got != want {
		t.Fatalf("DerivedURL = %q, want %q", got, want)
	}
	// A base without the trailing slash must not produce a double or missing one.
	if got := DerivedURL("https://cdn.example.com", orig, WidthSmall); got != "https://cdn.example.com/"+DerivedKey(orig, WidthSmall) {
		t.Fatalf("DerivedURL without trailing slash = %q", got)
	}
	if got := DerivedURL(testBase, "", WidthSmall); got != "" {
		t.Fatalf("DerivedURL of empty key = %q, want empty", got)
	}
}

func TestKeyFromURL(t *testing.T) {
	const key = "restaurants/u/photo.jpg"
	if got := KeyFromURL(testBase, testBase+key); got != key {
		t.Fatalf("KeyFromURL = %q, want %q", got, key)
	}
	// Foreign hosts must not yield a key: the old app still stores Supabase
	// URLs, and treating one as a bucket key would fabricate a path.
	if got := KeyFromURL(testBase, "https://wqwpreyqdmifcwftwnwe.supabase.co/storage/v1/object/public/restaurant-photos/x.jpg"); got != "" {
		t.Fatalf("KeyFromURL of a foreign URL = %q, want empty", got)
	}
	// A signed URL's query string is not part of the object key.
	if got := KeyFromURL(testBase, testBase+key+"?token=abc"); got != key {
		t.Fatalf("KeyFromURL with query = %q, want %q", got, key)
	}
}
