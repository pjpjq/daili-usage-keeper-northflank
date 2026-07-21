package app

import (
	"context"
	"fmt"
	"time"

	"github.com/sirupsen/logrus"
)

type ExternalPricingSyncer interface {
	SyncPricing(context.Context) error
}

type PricingSyncRunner struct {
	syncer   ExternalPricingSyncer
	interval time.Duration
	sleep    func(context.Context, time.Duration) bool
}

func NewPricingSyncRunner(syncer ExternalPricingSyncer, interval time.Duration) *PricingSyncRunner {
	return &PricingSyncRunner{
		syncer:   syncer,
		interval: interval,
		sleep:    maintenanceSleepContext,
	}
}

// Run performs one sync at startup and then refreshes prices at a fixed interval.
func (r *PricingSyncRunner) Run(ctx context.Context) error {
	if r == nil || r.syncer == nil {
		return fmt.Errorf("pricing syncer is nil")
	}
	if r.interval <= 0 {
		return fmt.Errorf("pricing sync interval must be positive")
	}
	if r.sleep == nil {
		r.sleep = maintenanceSleepContext
	}

	logrus.Info("external pricing sync task started")
	delay := time.Duration(0)
	for {
		if !r.sleep(ctx, delay) {
			return nil
		}
		if err := r.syncer.SyncPricing(ctx); err != nil {
			logrus.WithError(err).Error("external pricing sync failed; keeping existing prices")
		}
		delay = r.interval
	}
}
