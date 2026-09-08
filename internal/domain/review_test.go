package domain

import "testing"

func TestReviewStatusValid(t *testing.T) {
	if !ReviewPublished.Valid() || !ReviewHidden.Valid() {
		t.Fatal("published/hidden must be valid")
	}
	if ReviewStatus("deleted").Valid() {
		t.Fatal("unknown status must be invalid")
	}
	if ReviewStatus("").Valid() {
		t.Fatal("empty status must be invalid")
	}
}
