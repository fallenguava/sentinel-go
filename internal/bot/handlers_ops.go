package bot

import (
	"context"
	"fmt"
	"html"
	"strconv"
	"strings"
	"time"

	"sentinel-go/internal/config"
	"sentinel-go/internal/servicectl"
	"sentinel-go/internal/sysmon"
)

// handleStatus sends the current status
func (b *Bot) handleStatus(ctx context.Context, chatID string) {
	b.schedulerMu.RLock()
	defer b.schedulerMu.RUnlock()

	schedulerStatus := "🔴 Stopped"
	if b.schedulerActive {
		schedulerStatus = "🟢 Active"
	}

	enabledCount := 0
	enabledList := make([]string, 0, b.cfg.NumCams)
	for i := 1; i <= b.cfg.NumCams; i++ {
		if b.enabledCameras[i] {
			enabledCount++
			enabledList = append(enabledList, strconv.Itoa(i))
		}
	}

	msg := fmt.Sprintf(`📊 <b>Sentinel-GO Status</b>

🖥️ <b>DVR:</b> %s:%s
📷 <b>Total Cameras:</b> %d
✅ <b>Enabled:</b> %d (%s)

⏰ <b>Interval:</b> %d minutes
🔄 <b>Scheduler:</b> %s

🕐 <b>Current Time:</b> %s

Type /help for available commands.`,
		b.cfg.DVRIP, b.cfg.DVRPort,
		b.cfg.NumCams,
		enabledCount, strings.Join(enabledList, ", "),
		b.intervalMinutes,
		schedulerStatus,
		time.Now().Format("2006-01-02 15:04:05"))

	b.telegramClient.SendMessageToChat(ctx, chatID, msg)
}

func (b *Bot) handleSysInfo(ctx context.Context, chatID string) {
	snapshot, err := sysmon.Collect()
	if err != nil {
		b.telegramClient.SendMessageToChat(ctx, chatID, fmt.Sprintf("❌ Failed to collect system info: %v", err))
		return
	}

	msg := fmt.Sprintf(`🖥️ <b>System Info</b>

CPU: <b>%.1f%%</b>
RAM: <b>%s / %s</b> (%.1f%%)
Disk (/): <b>%s / %s</b> (%.1f%%)

Type /help for commands.`,
		snapshot.CPUPercent,
		formatBytes(snapshot.RAMUsed), formatBytes(snapshot.RAMTotal), snapshot.RAMPercent,
		formatBytes(snapshot.DiskUsed), formatBytes(snapshot.DiskTotal), snapshot.DiskPercent)

	b.telegramClient.SendMessageToChat(ctx, chatID, msg)
}

func (b *Bot) handleServices(ctx context.Context, chatID string) {
	statuses, err := servicectl.ListStatuses()
	if err != nil {
		b.telegramClient.SendMessageToChat(ctx, chatID, fmt.Sprintf("❌ Failed to list services: %v", err))
		return
	}

	lines := []string{"🧩 <b>Service Status</b>", ""}
	for _, st := range statuses {
		emoji := "⚪"
		switch st.Status {
		case "running":
			emoji = "🟢"
		case "stopped":
			emoji = "🟠"
		case "failed":
			emoji = "🔴"
		}
		lines = append(lines, fmt.Sprintf("%s <b>%s</b>: %s", emoji, st.Name, st.Status))
	}
	lines = append(lines, "", "Type /help for commands.")
	b.telegramClient.SendMessageToChat(ctx, chatID, strings.Join(lines, "\n"))
}

func (b *Bot) handleLogs(ctx context.Context, chatID string, args string) {
	service := strings.TrimSpace(args)
	allowedServices := strings.Join(servicectl.MonitoredServices(), ", ")
	if service == "" {
		b.telegramClient.SendMessageToChat(ctx, chatID,
			fmt.Sprintf("Usage: <code>/logs [service]</code>\n\nAllowed services:\n<code>%s</code>", allowedServices))
		return
	}

	if !servicectl.IsAllowedService(service) {
		b.telegramClient.SendMessageToChat(ctx, chatID,
			fmt.Sprintf("❌ Service not allowed: <b>%s</b>\n\nAllowed services:\n<code>%s</code>",
				html.EscapeString(service), allowedServices))
		return
	}

	logs, err := servicectl.GetLogs(service, 20)
	if err != nil {
		b.telegramClient.SendMessageToChat(ctx, chatID, fmt.Sprintf("❌ Failed to fetch logs for %s: %v", service, err))
		return
	}

	msg := fmt.Sprintf("📜 <b>Logs: %s</b>\n<code>%s</code>", html.EscapeString(service), html.EscapeString(logs))
	b.telegramClient.SendMessageToChat(ctx, chatID, msg)
}

func (b *Bot) handleServiceStart(ctx context.Context, chatID string, args string) {
	b.handleServiceAction(ctx, chatID, "start", strings.TrimSpace(args), servicectl.Start)
}

func (b *Bot) handleServiceStop(ctx context.Context, chatID string, args string) {
	b.handleServiceAction(ctx, chatID, "stop", strings.TrimSpace(args), servicectl.Stop)
}

func (b *Bot) handleServiceRestart(ctx context.Context, chatID string, args string) {
	b.handleServiceAction(ctx, chatID, "restart", strings.TrimSpace(args), servicectl.Restart)
}

func (b *Bot) handleServiceAction(ctx context.Context, chatID, action, service string, fn func(string) error) {
	allowedServices := strings.Join(servicectl.MonitoredServices(), ", ")
	if service == "" {
		b.telegramClient.SendMessageToChat(ctx, chatID,
			fmt.Sprintf("Usage: <code>/%s [service]</code>\n\nAllowed services:\n<code>%s</code>",
				action, allowedServices))
		return
	}

	if !servicectl.IsAllowedService(service) {
		b.telegramClient.SendMessageToChat(ctx, chatID,
			fmt.Sprintf("❌ Service not allowed: <b>%s</b>\n\nAllowed services:\n<code>%s</code>",
				html.EscapeString(service), allowedServices))
		return
	}

	if err := fn(service); err != nil {
		b.telegramClient.SendMessageToChat(ctx, chatID,
			fmt.Sprintf("❌ Failed to %s <b>%s</b>: %s", action, html.EscapeString(service), html.EscapeString(err.Error())))
		return
	}

	actionResult := action + "ed"
	if action == "stop" {
		actionResult = "stopped"
	}
	if action == "start" {
		actionResult = "started"
	}
	if action == "restart" {
		actionResult = "restarted"
	}

	status, err := servicectl.GetServiceStatus(service)
	if err != nil {
		b.telegramClient.SendMessageToChat(ctx, chatID,
			fmt.Sprintf("✅ Service <b>%s</b> %s.", html.EscapeString(service), actionResult))
		return
	}

	b.telegramClient.SendMessageToChat(ctx, chatID,
		fmt.Sprintf("✅ Service <b>%s</b> %s. Current status: <b>%s</b>", html.EscapeString(service), actionResult, html.EscapeString(status.Status)))
}

func formatBytes(v uint64) string {
	const unit = 1024
	if v < unit {
		return fmt.Sprintf("%d B", v)
	}
	div, exp := uint64(unit), 0
	for n := v / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(v)/float64(div), "KMGTPE"[exp])
}

// handleInterval sets or shows the capture interval
func (b *Bot) handleInterval(ctx context.Context, chatID string, args string) {
	if args == "" {
		b.schedulerMu.RLock()
		interval := b.intervalMinutes
		b.schedulerMu.RUnlock()

		b.telegramClient.SendMessageToChat(ctx, chatID,
			fmt.Sprintf("⏰ Current interval: <b>%d minutes</b>\n\nUsage: <code>/interval [minutes]</code>\n\nType /help for commands.", interval))
		return
	}

	minutes, err := strconv.Atoi(strings.TrimSpace(args))
	if err != nil || minutes < 1 {
		b.telegramClient.SendMessageToChat(ctx, chatID,
			"❌ Invalid interval. Please specify a positive number of minutes.\n\nType /help for commands.")
		return
	}

	if minutes > 1440 {
		b.telegramClient.SendMessageToChat(ctx, chatID,
			"❌ Interval cannot exceed 1440 minutes (24 hours).\n\nType /help for commands.")
		return
	}

	b.schedulerMu.Lock()
	oldInterval := b.intervalMinutes
	b.intervalMinutes = minutes
	b.schedulerMu.Unlock()

	b.restartScheduler()

	b.telegramClient.SendMessageToChat(ctx, chatID,
		fmt.Sprintf("✅ Interval changed: %d → <b>%d minutes</b>\n\nType /help for commands.", oldInterval, minutes))
}

// handleEnable enables cameras
func (b *Bot) handleEnable(ctx context.Context, chatID string, args string) {
	if args == "" {
		b.telegramClient.SendMessageToChat(ctx, chatID,
			"Usage: <code>/enable [camera(s)]</code>\n\nExamples:\n• /enable 1\n• /enable 1,3,5\n• /enable all\n\nType /help for commands.")
		return
	}

	cameras, err := config.ParseCameras(args, b.cfg.NumCams)
	if err != nil {
		b.telegramClient.SendMessageToChat(ctx, chatID,
			fmt.Sprintf("❌ Invalid camera selection: %v\n\nType /help for commands.", err))
		return
	}

	b.schedulerMu.Lock()
	for _, cam := range cameras {
		b.enabledCameras[cam] = true
	}
	b.schedulerMu.Unlock()

	camStrs := make([]string, len(cameras))
	for i, cam := range cameras {
		camStrs[i] = strconv.Itoa(cam)
	}

	b.telegramClient.SendMessageToChat(ctx, chatID,
		fmt.Sprintf("✅ Enabled camera(s): <b>%s</b>\n\nType /help for commands.", strings.Join(camStrs, ", ")))
}

// handleDisable disables cameras
func (b *Bot) handleDisable(ctx context.Context, chatID string, args string) {
	if args == "" {
		b.telegramClient.SendMessageToChat(ctx, chatID,
			"Usage: <code>/disable [camera(s)]</code>\n\nExamples:\n• /disable 1\n• /disable 1,3,5\n• /disable all\n\nType /help for commands.")
		return
	}

	cameras, err := config.ParseCameras(args, b.cfg.NumCams)
	if err != nil {
		b.telegramClient.SendMessageToChat(ctx, chatID,
			fmt.Sprintf("❌ Invalid camera selection: %v\n\nType /help for commands.", err))
		return
	}

	b.schedulerMu.Lock()
	for _, cam := range cameras {
		b.enabledCameras[cam] = false
	}
	b.schedulerMu.Unlock()

	camStrs := make([]string, len(cameras))
	for i, cam := range cameras {
		camStrs[i] = strconv.Itoa(cam)
	}

	b.telegramClient.SendMessageToChat(ctx, chatID,
		fmt.Sprintf("🚫 Disabled camera(s): <b>%s</b>\n\nType /help for commands.", strings.Join(camStrs, ", ")))
}

// handleList shows all cameras and their status
func (b *Bot) handleList(ctx context.Context, chatID string) {
	b.schedulerMu.RLock()
	defer b.schedulerMu.RUnlock()

	lines := make([]string, 0, b.cfg.NumCams+3)
	lines = append(lines, "📹 <b>Camera List</b>\n")

	for i := 1; i <= b.cfg.NumCams; i++ {
		status := "🔴"
		if b.enabledCameras[i] {
			status = "🟢"
		}
		name := b.cfg.GetCameraName(i)
		channel := i*100 + 1
		lines = append(lines, fmt.Sprintf("%s <b>%d.</b> %s (CH %d)", status, i, name, channel))
	}

	lines = append(lines, "\n🟢 = Enabled | 🔴 = Disabled")
	lines = append(lines, "\nType /help for commands.")

	b.telegramClient.SendMessageToChat(ctx, chatID, strings.Join(lines, "\n"))
}

// handleScheduler controls the scheduler
func (b *Bot) handleScheduler(ctx context.Context, chatID string, args string) {
	args = strings.ToLower(strings.TrimSpace(args))

	switch args {
	case "on", "start", "enable":
		b.schedulerMu.Lock()
		if b.schedulerActive {
			b.schedulerMu.Unlock()
			b.telegramClient.SendMessageToChat(ctx, chatID, "ℹ️ Scheduler is already running.\n\nType /help for commands.")
			return
		}
		b.schedulerActive = true
		b.schedulerMu.Unlock()
		b.restartScheduler()
		b.telegramClient.SendMessageToChat(ctx, chatID, "▶️ Scheduler <b>started</b>\n\nType /help for commands.")

	case "off", "stop", "disable":
		b.schedulerMu.Lock()
		if !b.schedulerActive {
			b.schedulerMu.Unlock()
			b.telegramClient.SendMessageToChat(ctx, chatID, "ℹ️ Scheduler is already stopped.\n\nType /help for commands.")
			return
		}
		b.schedulerActive = false
		b.schedulerMu.Unlock()
		select {
		case b.stopScheduler <- struct{}{}:
		default:
		}
		b.telegramClient.SendMessageToChat(ctx, chatID, "⏹️ Scheduler <b>stopped</b>\n\nType /help for commands.")

	default:
		b.schedulerMu.RLock()
		status := "🔴 Stopped"
		if b.schedulerActive {
			status = "🟢 Active"
		}
		b.schedulerMu.RUnlock()

		b.telegramClient.SendMessageToChat(ctx, chatID,
			fmt.Sprintf("🔄 <b>Scheduler:</b> %s\n\nUsage:\n• <code>/scheduler on</code> - Start\n• <code>/scheduler off</code> - Stop\n\nType /help for commands.", status))
	}
}
