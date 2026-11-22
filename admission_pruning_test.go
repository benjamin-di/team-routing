package main

import (
	"fmt"
	"testing"
	"time"
)

func TestReconcileAdmissionPrunesCoveredCompletionsAndPreservesSettlementWindow(t *testing.T) {
	_, c, _, now := admissionFixture(t)
	pollAt := now.Add(5 * time.Minute)
	for i := range 150 {
		recordAdmissionCompletion(c.ID, fmt.Sprintf("old-%d", i), now.Add(time.Duration(i)*time.Second))
	}
	recordAdmissionCompletion(c.ID, "edge-before", pollAt.Add(-10*time.Second))
	recordAdmissionCompletion(c.ID, "edge-at", pollAt)
	recordAdmissionCompletion(c.ID, "after", pollAt.Add(5*time.Second))

	reconcileAdmission(c.ID, pollAt)

	admission.Lock()
	remaining := len(admission.Data.Completed)
	admission.Unlock()
	if remaining != 3 {
		t.Fatalf("expected 3 unreconciled completions (settlement window + post-poll), got %d", remaining)
	}
	// Reload from disk to confirm pruned state was persisted.
	admission.Lock()
	admission.Path = ""
	admission.Unlock()
	admission.Lock()
	loadAdmission()
	reloaded := len(admission.Data.Completed)
	admission.Unlock()
	if reloaded != 3 {
		t.Fatalf("persisted ledger still retained covered completions: got %d", reloaded)
	}
}

func TestReconcileAdmissionPreservesCompletionsNeededByOlderActiveSignal(t *testing.T) {
	_, c, q, now := admissionFixture(t)
	signalAt := now.Add(2 * time.Minute)
	pollAt := now.Add(5 * time.Minute)
	recordAdmissionSignals(c.ID, quotaHeaders(.5, .3, q.Groups[0].Buckets[0].Reset, q.Groups[0].Buckets[1].Reset), signalAt, signalAt)
	recordAdmissionCompletion(c.ID, "before-signal", signalAt.Add(-time.Minute))
	recordAdmissionCompletion(c.ID, "between-signal-and-poll", signalAt.Add(time.Minute))
	recordAdmissionCompletion(c.ID, "after-poll", pollAt.Add(time.Second))

	reconcileAdmission(c.ID, pollAt)

	admission.Lock()
	remaining := len(admission.Data.Completed)
	admission.Unlock()
	if remaining != 2 {
		t.Fatalf("expected 2 completions preserved for older active signal, got %d", remaining)
	}
}
