package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type externalPricingSyncStub struct {
	calls int
	errs  []error
}

func (s *externalPricingSyncStub) SyncPricing(context.Context) error {
	s.calls++
	if s.calls <= len(s.errs) {
		return s.errs[s.calls-1]
	}
	return nil
}

func TestPricingSyncRunnerRunsImmediatelyThenAtInterval(t *testing.T) {
	syncer := &externalPricingSyncStub{}
	runner := NewPricingSyncRunner(syncer, 6*time.Hour)
	var delays []time.Duration
	runner.sleep = func(_ context.Context, delay time.Duration) bool {
		delays = append(delays, delay)
		return len(delays) < 3
	}

	if err := runner.Run(context.Background()); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if syncer.calls != 2 {
		t.Fatalf("expected two pricing sync calls, got %d", syncer.calls)
	}
	expected := []time.Duration{0, 6 * time.Hour, 6 * time.Hour}
	for i, delay := range delays {
		if delay != expected[i] {
			t.Fatalf("expected delay %d to be %s, got %s", i, expected[i], delay)
		}
	}
}

func TestPricingSyncRunnerKeepsRunningAfterFailure(t *testing.T) {
	logs := captureAppInfoLogs(t)
	syncer := &externalPricingSyncStub{errs: []error{errors.New("catalog unavailable")}}
	runner := NewPricingSyncRunner(syncer, time.Hour)
	sleepCalls := 0
	runner.sleep = func(context.Context, time.Duration) bool {
		sleepCalls++
		return sleepCalls < 3
	}

	if err := runner.Run(context.Background()); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if syncer.calls != 2 {
		t.Fatalf("expected runner to retry after failure, got %d calls", syncer.calls)
	}
	if content := logs.String(); !strings.Contains(content, "keeping existing prices") {
		t.Fatalf("expected fail-open pricing log, got %q", content)
	}
}

func TestPricingSyncRunnerValidatesConfig(t *testing.T) {
	if err := NewPricingSyncRunner(nil, time.Hour).Run(context.Background()); err == nil {
		t.Fatal("expected nil syncer validation error")
	}
	if err := NewPricingSyncRunner(&externalPricingSyncStub{}, 0).Run(context.Background()); err == nil {
		t.Fatal("expected interval validation error")
	}
}
