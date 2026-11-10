package main

import "testing"

func TestCombineQuota(t *testing.T) {
	total := func(v float64) *float64 { return &v }
	plan := &Quota{Total: total(1000), Used: total(250), Remaining: total(750), Unit: "credits"}
	addOn := &Quota{Total: total(0), Used: total(0), Remaining: total(0)}
	packages := []Quota{
		{Total: total(500), Used: total(100), Remaining: total(400), Unit: "credits"},
		{Total: total(100), Used: total(0), Remaining: total(100), IsQuotaExceeded: true},
	}
	got := combineQuota(plan, addOn, packages)
	if got == nil {
		t.Fatal("combineQuota returned nil")
	}
	if got.Total == nil || *got.Total != 1500 {
		t.Fatalf("total = %v, want 1500", got.Total)
	}
	if got.Used == nil || *got.Used != 350 {
		t.Fatalf("used = %v, want 350", got.Used)
	}
	if got.Remaining == nil || *got.Remaining != 1150 {
		t.Fatalf("remaining = %v, want 1150", got.Remaining)
	}
	if got.Percentage == nil || *got.Percentage < 0.2333 || *got.Percentage > 0.2334 {
		t.Fatalf("percentage = %v, want ~0.2333", got.Percentage)
	}

	if combineQuota(nil, nil, nil) != nil {
		t.Fatal("empty inputs should return nil")
	}
}

func TestNormalizeTime(t *testing.T) {
	if got := normalizeTime(0); got != 0 {
		t.Fatalf("normalizeTime(0) = %d", got)
	}
	if got := normalizeTime(1700000000); got != 1700000000000 {
		t.Fatalf("seconds normalize = %d", got)
	}
	if got := normalizeTime(1700000000000); got != 1700000000000 {
		t.Fatalf("millis normalize = %d", got)
	}
	if got := normalizeTime("9999-12-31T23:59:59Z"); got != 0 {
		t.Fatalf("permanent expiry = %d, want 0", got)
	}
	if got := normalizeTime("2024-01-02T03:04:05Z"); got == 0 {
		t.Fatal("iso parse failed")
	}
}
