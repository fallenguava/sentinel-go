package servicectl

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const commandTimeout = 10 * time.Second

var monitoredServices = []string{
	"aurexa-fastify",
	"sentinel-go",
	"gitea",
	"gitea-runner",
	"postgresql",
	"cloudflared",
	"netdata",
}

// ServiceStatus stores normalized and raw service status values.
type ServiceStatus struct {
	Name       string
	Status     string
	RawStatus  string
	StatusLine string
}

// MonitoredServices returns the list of services managed by the bot.
func MonitoredServices() []string {
	services := make([]string, len(monitoredServices))
	copy(services, monitoredServices)
	return services
}

// IsAllowedService validates that a service is in the allowlist.
func IsAllowedService(service string) bool {
	service = strings.TrimSpace(service)
	for _, allowed := range monitoredServices {
		if service == allowed {
			return true
		}
	}
	return false
}

// GetServiceStatus returns the current state of one service.
func GetServiceStatus(service string) (*ServiceStatus, error) {
	if !IsAllowedService(service) {
		return nil, fmt.Errorf("service '%s' is not allowed", service)
	}

	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sudo", "-n", "systemctl", "is-active", service)
	out, err := cmd.CombinedOutput()
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		raw = "unknown"
	}

	status := normalizeStatus(raw)
	if err != nil && raw == "unknown" {
		return nil, fmt.Errorf("failed to check status for %s: %w", service, err)
	}

	return &ServiceStatus{
		Name:       service,
		Status:     status,
		RawStatus:  raw,
		StatusLine: fmt.Sprintf("%s: %s", service, status),
	}, nil
}

// ListStatuses returns status information for all monitored services.
func ListStatuses() ([]*ServiceStatus, error) {
	services := MonitoredServices()
	statuses := make([]*ServiceStatus, 0, len(services))

	for _, service := range services {
		status, err := GetServiceStatus(service)
		if err != nil {
			return nil, err
		}
		statuses = append(statuses, status)
	}

	return statuses, nil
}

// Start starts a monitored service.
func Start(service string) error {
	return runSystemctlAction("start", service)
}

// Stop stops a monitored service.
func Stop(service string) error {
	return runSystemctlAction("stop", service)
}

// Restart restarts a monitored service.
func Restart(service string) error {
	return runSystemctlAction("restart", service)
}

// GetLogs returns the last N lines from journalctl for a monitored service.
func GetLogs(service string, lines int) (string, error) {
	if !IsAllowedService(service) {
		return "", fmt.Errorf("service '%s' is not allowed", service)
	}
	if lines <= 0 {
		lines = 20
	}

	cmd := exec.Command("journalctl", "-u", service, "-n", fmt.Sprintf("%d", lines), "--no-pager")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to read logs for %s: %w", service, err)
	}

	logs := strings.TrimSpace(string(out))
	if logs == "" {
		return "(no logs)", nil
	}

	return logs, nil
}

func runSystemctlAction(action, service string) error {
	if !IsAllowedService(service) {
		return fmt.Errorf("service '%s' is not allowed", service)
	}

	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sudo", "-n", "systemctl", action, service)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return fmt.Errorf("failed to %s %s: %s", action, service, errMsg)
		}
		return fmt.Errorf("failed to %s %s: %w", action, service, err)
	}

	return nil
}

func normalizeStatus(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "active":
		return "running"
	case "inactive":
		return "stopped"
	case "failed":
		return "failed"
	default:
		return raw
	}
}
