package service

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestUsageOverviewAllCacheBuildsOnceForConcurrentInitialRequests(t *testing.T) {
	now := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)
	var builds atomic.Int32
	buildStarted := make(chan struct{})
	releaseBuild := make(chan struct{})
	var startedOnce sync.Once
	provider := &usageService{
		now: func() time.Time { return now },
		buildOverview: func(UsageFilter) (*UsageOverviewSnapshot, error) {
			builds.Add(1)
			startedOnce.Do(func() { close(buildStarted) })
			<-releaseBuild
			return usageOverviewCacheTestSnapshot(1), nil
		},
	}

	const callers = 16
	start := make(chan struct{})
	results := make(chan *UsageOverviewSnapshot, callers)
	errors := make(chan error, callers)
	for range callers {
		go func() {
			<-start
			overview, err := provider.GetUsageOverview(context.Background(), UsageFilter{Range: "all"})
			results <- overview
			errors <- err
		}()
	}
	close(start)
	<-buildStarted
	close(releaseBuild)

	for range callers {
		if err := <-errors; err != nil {
			t.Fatalf("GetUsageOverview returned error: %v", err)
		}
		if overview := <-results; overview == nil || overview.Summary.RequestCount != 1 {
			t.Fatalf("expected cached overview with one request, got %+v", overview)
		}
	}
	if got := builds.Load(); got != 1 {
		t.Fatalf("expected concurrent initial requests to share one build, got %d", got)
	}
}

func TestUsageOverviewAllCacheReturnsStaleAndStartsSingleRefresh(t *testing.T) {
	now := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)
	var builds atomic.Int32
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	provider := &usageService{
		now: func() time.Time { return now },
		buildOverview: func(UsageFilter) (*UsageOverviewSnapshot, error) {
			build := builds.Add(1)
			if build == 1 {
				return usageOverviewCacheTestSnapshot(1), nil
			}
			if build == 2 {
				close(refreshStarted)
				<-releaseRefresh
				return usageOverviewCacheTestSnapshot(2), nil
			}
			return usageOverviewCacheTestSnapshot(build), nil
		},
	}

	initial, err := provider.GetUsageOverview(context.Background(), UsageFilter{Range: "all"})
	if err != nil || initial.Summary.RequestCount != 1 {
		t.Fatalf("unexpected initial overview: overview=%+v err=%v", initial, err)
	}

	now = now.Add(usageOverviewCacheTTL + time.Second)
	stale, err := provider.GetUsageOverview(context.Background(), UsageFilter{Range: "all"})
	if err != nil || stale.Summary.RequestCount != 1 {
		t.Fatalf("expected expired cache to return stale value: overview=%+v err=%v", stale, err)
	}
	<-refreshStarted

	provider.overviewAllCache.mu.Lock()
	refreshDone := provider.overviewAllCache.ready
	provider.overviewAllCache.mu.Unlock()

	const callers = 16
	results := make(chan *UsageOverviewSnapshot, callers)
	for range callers {
		go func() {
			overview, callErr := provider.GetUsageOverview(context.Background(), UsageFilter{Range: "all"})
			if callErr != nil {
				t.Errorf("concurrent stale GetUsageOverview returned error: %v", callErr)
			}
			results <- overview
		}()
	}
	for range callers {
		if overview := <-results; overview == nil || overview.Summary.RequestCount != 1 {
			t.Fatalf("expected concurrent request to receive stale value, got %+v", overview)
		}
	}
	if got := builds.Load(); got != 2 {
		t.Fatalf("expected one in-flight background refresh, got %d total builds", got)
	}

	close(releaseRefresh)
	<-refreshDone
	refreshed, err := provider.GetUsageOverview(context.Background(), UsageFilter{Range: "all"})
	if err != nil || refreshed.Summary.RequestCount != 2 {
		t.Fatalf("expected refreshed cache value: overview=%+v err=%v", refreshed, err)
	}
	if got := builds.Load(); got != 2 {
		t.Fatalf("expected refresh to remain single-flight, got %d total builds", got)
	}
}

func TestUsageOverviewAllCacheOnlyCachesUnboundedAllRange(t *testing.T) {
	now := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)
	var builds atomic.Int32
	provider := &usageService{
		now: func() time.Time { return now },
		buildOverview: func(UsageFilter) (*UsageOverviewSnapshot, error) {
			return usageOverviewCacheTestSnapshot(builds.Add(1)), nil
		},
	}

	if _, err := provider.GetUsageOverview(context.Background(), UsageFilter{Range: "all"}); err != nil {
		t.Fatalf("unbounded GetUsageOverview returned error: %v", err)
	}
	if _, err := provider.GetUsageOverview(context.Background(), UsageFilter{Range: "all"}); err != nil {
		t.Fatalf("cached GetUsageOverview returned error: %v", err)
	}
	start := now.Add(-time.Hour)
	if _, err := provider.GetUsageOverview(context.Background(), UsageFilter{Range: "all", StartTime: &start}); err != nil {
		t.Fatalf("bounded GetUsageOverview returned error: %v", err)
	}
	if _, err := provider.GetUsageOverview(context.Background(), UsageFilter{Range: "24h"}); err != nil {
		t.Fatalf("non-all GetUsageOverview returned error: %v", err)
	}
	if got := builds.Load(); got != 3 {
		t.Fatalf("expected only the unbounded all-range request to be cached, got %d builds", got)
	}
}

func usageOverviewCacheTestSnapshot(requestCount int32) *UsageOverviewSnapshot {
	return &UsageOverviewSnapshot{Summary: UsageOverviewSummary{RequestCount: int64(requestCount)}}
}
