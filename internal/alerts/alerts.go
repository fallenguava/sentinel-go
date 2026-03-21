package alerts

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"sentinel-go/internal/servicectl"
	"sentinel-go/internal/sysmon"
	"sentinel-go/internal/telegram"
)

const (
	defaultInterval = 60 * time.Second
	cpuThreshold    = 80.0
	ramThreshold    = 85.0
	diskThreshold   = 90.0
)

// Manager performs periodic health checks and sends one-shot alerts per incident.
type Manager struct {
	telegramClient *telegram.Client
	interval       time.Duration
	activeAlerts   map[string]bool
}

// NewManager creates a new alert manager.
func NewManager(telegramClient *telegram.Client, interval time.Duration) *Manager {
	if interval <= 0 {
		interval = defaultInterval
	}

	return &Manager{
		telegramClient: telegramClient,
		interval:       interval,
		activeAlerts:   make(map[string]bool),
	}
}

// Start begins the periodic alert loop.
func (m *Manager) Start(ctx context.Context) {
	log.Printf("[ALERTS] Starting monitor loop (interval=%s)", m.interval)
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	m.checkAndNotify(ctx)

	for {
		select {
		case <-ctx.Done():
			log.Println("[ALERTS] Stopping monitor loop")
			return
		case <-ticker.C:
			m.checkAndNotify(ctx)
		}
	}
}

func (m *Manager) checkAndNotify(ctx context.Context) {
	statuses, err := servicectl.ListStatuses()
	if err != nil {
		log.Printf("[ALERTS] Failed to list service statuses: %v", err)
	} else {
		for _, status := range statuses {
			key := "svc:" + status.Name
			isIncident := strings.ToLower(status.Status) != "running"
			if isIncident {
				msg := fmt.Sprintf("🚨 <b>Service Alert</b>\n\nService <b>%s</b> is <b>%s</b> (raw: %s)", status.Name, status.Status, status.RawStatus)
				m.raiseAlert(ctx, key, msg)
			} else {
				m.resolveAlert(key)
			}
		}
	}

	snapshot, err := sysmon.Collect()
	if err != nil {
		log.Printf("[ALERTS] Failed to collect system metrics: %v", err)
		return
	}

	if snapshot.CPUPercent > cpuThreshold {
		m.raiseAlert(ctx, "cpu", fmt.Sprintf("🚨 <b>CPU Alert</b>\n\nCPU usage is <b>%.1f%%</b> (threshold %.0f%%)", snapshot.CPUPercent, cpuThreshold))
	} else {
		m.resolveAlert("cpu")
	}

	if snapshot.RAMPercent > ramThreshold {
		m.raiseAlert(ctx, "ram", fmt.Sprintf("🚨 <b>RAM Alert</b>\n\nRAM usage is <b>%.1f%%</b> (threshold %.0f%%)", snapshot.RAMPercent, ramThreshold))
	} else {
		m.resolveAlert("ram")
	}

	if snapshot.DiskPercent > diskThreshold {
		m.raiseAlert(ctx, "disk", fmt.Sprintf("🚨 <b>Disk Alert</b>\n\nRoot disk usage is <b>%.1f%%</b> (threshold %.0f%%)", snapshot.DiskPercent, diskThreshold))
	} else {
		m.resolveAlert("disk")
	}
}

func (m *Manager) raiseAlert(ctx context.Context, key, message string) {
	if m.activeAlerts[key] {
		return
	}

	if err := m.telegramClient.SendMessage(ctx, message); err != nil {
		log.Printf("[ALERTS] Failed to send alert %s: %v", key, err)
		return
	}

	m.activeAlerts[key] = true
	log.Printf("[ALERTS] Alert raised: %s", key)
}

func (m *Manager) resolveAlert(key string) {
	if !m.activeAlerts[key] {
		return
	}
	delete(m.activeAlerts, key)
	log.Printf("[ALERTS] Alert resolved: %s", key)
}
