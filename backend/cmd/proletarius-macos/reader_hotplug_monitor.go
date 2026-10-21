package main

import (
	"context"
	"log"
	"sync"
	"time"
)

// Keep hardware-session teardown out of GET /api/v2/device-snapshot. A short
// interval preserves prompt unplug handling, while two independent misses
// avoid treating a transient PC/SC or BLE status failure as a physical loss.
const (
	readerHotplugPollInterval         = 2 * time.Second
	readerHotplugScanTimeout          = 1 * time.Second
	readerHotplugMissingConfirmations = 2
)

type readerHotplugMonitor struct {
	manager     *readerManager
	listReaders func(context.Context) ([]string, error)
	stopSession func(uint64)
	required    int
	pollMu      sync.Mutex
}

func (m *readerHotplugMonitor) poll(parent context.Context) {
	if m.manager == nil || m.listReaders == nil {
		return
	}
	// A disconnected app must not wake PC/SC every two seconds just to prove
	// it is still disconnected. The selected reader is the monitor's sole
	// activation condition.
	if !m.manager.hasSelectedReader() {
		return
	}
	m.pollMu.Lock()
	defer m.pollMu.Unlock()
	// Another caller may have disconnected the reader while this poll waited.
	if !m.manager.hasSelectedReader() {
		return
	}
	ctx, cancel := context.WithTimeout(parent, readerHotplugScanTimeout)
	defer cancel()
	readers, err := m.listReaders(ctx)
	if err != nil {
		// A scanner/bridge failure is not evidence of physical unplug. The
		// next successful sample restarts normal observation without tearing
		// down a working reader session.
		return
	}
	removedGeneration, removed := m.manager.reconcileHotUnplug(readers, m.required)
	if !removed {
		return
	}
	if m.stopSession != nil {
		m.stopSession(removedGeneration)
	}
}

func (m *readerHotplugMonitor) run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = readerHotplugPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.poll(ctx)
		}
	}
}

func (a *app) startReaderHotplugMonitor(ctx context.Context) {
	if a == nil || a.readerMgr == nil {
		return
	}
	monitor := &readerHotplugMonitor{
		manager:     a.readerMgr,
		listReaders: a.readerMgr.hotplugInventory,
		stopSession: func(generation uint64) {
			stopVoWiFiForReaderGeneration(generation)
			a.multiStandby().invalidateReaderGeneration(generation)
		},
		required: readerHotplugMissingConfirmations,
	}
	go func() {
		log.Printf("reader hot-unplug monitor started (interval=%s confirmations=%d)",
			readerHotplugPollInterval, readerHotplugMissingConfirmations)
		monitor.run(ctx, readerHotplugPollInterval)
	}()
}
