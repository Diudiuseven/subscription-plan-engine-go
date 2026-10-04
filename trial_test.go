package subscription

import (
	"testing"
	"time"
)

func TestTrialStartsAndExpiresIntoActive(t *testing.T) {
	eng, clock, gw, _ := newTestEngine(t, defaultConfig())
	if _, err := eng.StartTrial(StartTrialRequest{RequestID: "r1", AccountID: "a1", PlanID: "basic"}); err != nil {
		t.Fatal(err)
	}
	s, _ := eng.GetSubscription("a1")
	if s.Status != StatusTrialing {
		t.Fatalf("want trialing, got %s", s.Status)
	}
	if s.TrialEnd == nil || !s.TrialEnd.Equal(t0.AddDate(0, 0, 14)) {
		t.Fatalf("trial end wrong: %v", s.TrialEnd)
	}
	ent, _ := eng.Entitlement("a1")
	if !ent.Entitled || ent.PlanID != "basic" {
		t.Fatalf("trial should be entitled to basic: %+v", ent)
	}

	// Advance exactly to trial end: conversion happens deterministically.
	clock.Set(t0.AddDate(0, 0, 14))
	if err := eng.ProcessDue(); err != nil {
		t.Fatal(err)
	}
	s, _ = eng.GetSubscription("a1")
	if s.Status != StatusActive {
		t.Fatalf("want active after trial expiry, got %s", s.Status)
	}
	if s.TrialEnd != nil {
		t.Fatal("trial end should be cleared")
	}
	if !s.CurrentPeriodStart.Equal(t0.AddDate(0, 0, 14)) {
		t.Fatalf("paid period should start at trial end, got %v", s.CurrentPeriodStart)
	}
	if got := gw.total(); got != 3000 {
		t.Fatalf("want first charge 3000, got %d", got)
	}

	// Processing again must not migrate the state a second time.
	if err := eng.ProcessDue(); err != nil {
		t.Fatal(err)
	}
	s2, _ := eng.GetSubscription("a1")
	if s2.Version != s.Version {
		t.Fatalf("repeat trigger caused a second migration: %+v", s2)
	}
	if got := gw.total(); got != 3000 {
		t.Fatalf("repeat trigger double-charged: %d", got)
	}
}

func TestManualConvertIsIdempotent(t *testing.T) {
	eng, clock, gw, _ := newTestEngine(t, defaultConfig())
	if _, err := eng.StartTrial(StartTrialRequest{RequestID: "r1", AccountID: "a1", PlanID: "pro"}); err != nil {
		t.Fatal(err)
	}
	clock.Advance(3 * 24 * time.Hour)
	s1, err := eng.ConvertTrial(ConvertTrialRequest{RequestID: "conv-1", AccountID: "a1"})
	if err != nil {
		t.Fatal(err)
	}
	if s1.Status != StatusActive {
		t.Fatalf("want active, got %s", s1.Status)
	}
	// Duplicate request ID: same result, no second charge.
	s2, err := eng.ConvertTrial(ConvertTrialRequest{RequestID: "conv-1", AccountID: "a1"})
	if err != nil {
		t.Fatal(err)
	}
	if s2.Version != s1.Version {
		t.Fatalf("duplicate convert mutated state: v%d -> v%d", s1.Version, s2.Version)
	}
	if got := gw.total(); got != 9000 {
		t.Fatalf("duplicate convert double-charged: %d", got)
	}
	// Same request ID reused for a different operation key is rejected.
	if _, err := eng.ConvertTrial(ConvertTrialRequest{RequestID: "r1", AccountID: "a1"}); err == nil {
		t.Fatal("reused request ID should be rejected")
	}
}

func TestConvertAfterTrialLapseStartsAtTrialEnd(t *testing.T) {
	eng, clock, _, _ := newTestEngine(t, defaultConfig())
	if _, err := eng.StartTrial(StartTrialRequest{RequestID: "r1", AccountID: "a1", PlanID: "basic"}); err != nil {
		t.Fatal(err)
	}
	// Convert 2 days after trial end without ProcessDue in between.
	clock.Set(t0.AddDate(0, 0, 16))
	s, err := eng.ConvertTrial(ConvertTrialRequest{RequestID: "c1", AccountID: "a1"})
	if err != nil {
		t.Fatal(err)
	}
	if !s.CurrentPeriodStart.Equal(t0.AddDate(0, 0, 14)) {
		t.Fatalf("period should start at trial end, got %v", s.CurrentPeriodStart)
	}
}
