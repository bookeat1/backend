package otpcode

import (
	"regexp"
	"testing"
)

func TestGenerateIsSixDigits(t *testing.T) {
	re := regexp.MustCompile(`^\d{6}$`)
	for i := 0; i < 50; i++ {
		c, err := Generate()
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if !re.MatchString(c) {
			t.Fatalf("Generate() = %q, want 6 digits", c)
		}
	}
}

func TestHashIsStableAndDistinct(t *testing.T) {
	if Hash("123456") != Hash("123456") {
		t.Error("Hash must be deterministic")
	}
	if Hash("123456") == Hash("654321") {
		t.Error("different codes must hash differently")
	}
}
