package bot

import (
	"context"
	"fmt"
	"html"
	"log"
	"strconv"
	"strings"
	"time"
)

// handleStart sends welcome message for authenticated users
func (b *Bot) handleStart(ctx context.Context, chatID string) {
	chatIDInt, _ := strconv.ParseInt(chatID, 10, 64)
	user, _ := b.db.GetUser(ctx, chatIDInt)

	name := "User"
	if user != nil {
		name = user.Name
	}

	msg := fmt.Sprintf(`🏠 <b>Welcome back, %s!</b>

📷 Monitoring <b>%d cameras</b> on %s
⏰ Capture interval: <b>%d minutes</b>

Type /help to see all available commands.`, name, b.cfg.NumCams, b.cfg.DVRIP, b.intervalMinutes)

	b.telegramClient.SendMessageToChat(ctx, chatID, msg)
}

// handleHelp sends the help message
func (b *Bot) handleHelp(ctx context.Context, chatID, role string) {
	if role == "admin" {
		help := `📋 <b>Sentinel-GO Commands (Admin)</b>

<b>📷 Capture:</b>
/capture [cams] - Capture snapshot(s)
  <code>/capture</code> - All enabled cameras
  <code>/capture 1</code> - Camera 1 only
  <code>/capture 1,3,5</code> - Cameras 1, 3, 5
  <code>/capture 1-4</code> - Cameras 1 to 4
  <code>/capture all</code> - All cameras
  <i>Shortcuts: /c, /snap</i>

/cam - Capture laptop webcam (on-demand)
  <i>Shortcut: /webcam</i>
	<code>/cam gif</code> - Capture as animated GIF (fast)
	<code>/cam list</code> - Show last 20 webcam captures
	<code>/cam show [n]</code> - Send capture by list number

<b>⚙️ Settings:</b>
/interval [min] - Set capture interval
  <code>/interval 30</code> - Every 30 min
  <i>Shortcut: /int</i>

/enable [cams] - Enable camera(s)
/disable [cams] - Disable camera(s)
  <i>Shortcuts: /on, /off</i>

<b>📊 Info:</b>
/status - Current settings
/list - List all cameras
/whoami - Your profile
/ping - Check bot status
/sysinfo - CPU, RAM, disk usage
	<i>Shortcut: /sys</i>
/services - Monitored service states
	<i>Shortcut: /svc</i>
/logs [service] - Last 20 log lines

<b>🛠️ Service Control:</b>
/start [service] - Start service
/stop [service] - Stop service
/restart [service] - Restart service
	<i>Shortcut: /res</i>

Allowed services:
<code>aurexa-fastify, sentinel-go, gitea, gitea-runner, postgresql, cloudflared, netdata</code>

<b>🔄 Scheduler:</b>
/scheduler [on/off] - Control auto-capture
  <i>Shortcut: /sched</i>

<b>👑 Admin:</b>
/approve [chat_id] - Approve registration
/reject [chat_id] - Reject registration
/users - List approved users
/revoke [chat_id] - Revoke user access
/promote [chat_id] - Promote to admin

Type /help anytime to see this menu.`

		b.telegramClient.SendMessageToChat(ctx, chatID, help)
		return
	}

	help := `📋 <b>Sentinel-GO Commands</b>

<b>📷 Capture:</b>
/capture [cams] - Capture snapshot(s)
  <i>Shortcuts: /c, /snap</i>

/cam - Capture laptop webcam (on-demand)
  <i>Shortcut: /webcam</i>
	<code>/cam gif</code> - Capture as animated GIF (fast)
	<code>/cam list</code> - Show last 20 webcam captures
	<code>/cam show [n]</code> - Send capture by list number

<b>⚙️ Settings:</b>
/interval [min] - Set capture interval
  <i>Shortcut: /int</i>
/enable [cams] - Enable camera(s)
/disable [cams] - Disable camera(s)
  <i>Shortcuts: /on, /off</i>

<b>📊 Info:</b>
/status - Current settings
/list - List all cameras
/whoami - Your profile
/ping - Check bot status

<b>🔄 Scheduler:</b>
/scheduler [on/off] - Control auto-capture
  <i>Shortcut: /sched</i>

Type /help anytime to see this menu.`

	b.telegramClient.SendMessageToChat(ctx, chatID, help)
}

func (b *Bot) handleApprove(ctx context.Context, chatID, args string) {
	targetChatID, err := parseChatIDArg(args)
	if err != nil {
		b.telegramClient.SendMessageToChat(ctx, chatID, "Usage: <code>/approve [chat_id]</code>")
		return
	}

	ok, err := b.db.ApproveUser(ctx, targetChatID)
	if err != nil {
		b.telegramClient.SendMessageToChat(ctx, chatID, fmt.Sprintf("❌ Failed to approve: %v", err))
		return
	}
	if !ok {
		b.telegramClient.SendMessageToChat(ctx, chatID, "❌ User not found.")
		return
	}

	targetChat := strconv.FormatInt(targetChatID, 10)
	_ = b.telegramClient.SendMessageToChat(ctx, targetChat, "✅ Your access has been approved! Send /help to get started.")
	b.telegramClient.SendMessageToChat(ctx, chatID, fmt.Sprintf("✅ Approved chat_id %d", targetChatID))
}

func (b *Bot) handleReject(ctx context.Context, chatID, args string) {
	targetChatID, err := parseChatIDArg(args)
	if err != nil {
		b.telegramClient.SendMessageToChat(ctx, chatID, "Usage: <code>/reject [chat_id]</code>")
		return
	}

	ok, err := b.db.RejectUser(ctx, targetChatID)
	if err != nil {
		b.telegramClient.SendMessageToChat(ctx, chatID, fmt.Sprintf("❌ Failed to reject: %v", err))
		return
	}
	if !ok {
		b.telegramClient.SendMessageToChat(ctx, chatID, "❌ User not found.")
		return
	}

	targetChat := strconv.FormatInt(targetChatID, 10)
	_ = b.telegramClient.SendMessageToChat(ctx, targetChat, "❌ Your access request was rejected.")
	b.telegramClient.SendMessageToChat(ctx, chatID, fmt.Sprintf("✅ Rejected chat_id %d", targetChatID))
}

func (b *Bot) handleUsers(ctx context.Context, chatID string) {
	users, err := b.db.ListApprovedUsers(ctx)
	if err != nil {
		b.telegramClient.SendMessageToChat(ctx, chatID, fmt.Sprintf("❌ Failed to list users: %v", err))
		return
	}

	if len(users) == 0 {
		b.telegramClient.SendMessageToChat(ctx, chatID, "ℹ️ No approved users found.")
		return
	}

	lines := []string{"👥 <b>Approved Users</b>", ""}
	for _, user := range users {
		lines = append(lines, fmt.Sprintf("• %s (%d) - %s", html.EscapeString(user.Name), user.ChatID, user.Role))
	}
	b.telegramClient.SendMessageToChat(ctx, chatID, strings.Join(lines, "\n"))
}

func (b *Bot) handleRevoke(ctx context.Context, chatID, args string) {
	targetChatID, err := parseChatIDArg(args)
	if err != nil {
		b.telegramClient.SendMessageToChat(ctx, chatID, "Usage: <code>/revoke [chat_id]</code>")
		return
	}

	ok, err := b.db.RevokeUser(ctx, targetChatID)
	if err != nil {
		b.telegramClient.SendMessageToChat(ctx, chatID, fmt.Sprintf("❌ Failed to revoke: %v", err))
		return
	}
	if !ok {
		b.telegramClient.SendMessageToChat(ctx, chatID, "❌ User not found.")
		return
	}

	targetChat := strconv.FormatInt(targetChatID, 10)
	_ = b.telegramClient.SendMessageToChat(ctx, targetChat, "❌ Your access has been revoked.")
	b.telegramClient.SendMessageToChat(ctx, chatID, fmt.Sprintf("✅ Revoked chat_id %d", targetChatID))
}

func (b *Bot) handlePromote(ctx context.Context, chatID, args string) {
	targetChatID, err := parseChatIDArg(args)
	if err != nil {
		b.telegramClient.SendMessageToChat(ctx, chatID, "Usage: <code>/promote [chat_id]</code>")
		return
	}

	ok, err := b.db.PromoteUser(ctx, targetChatID)
	if err != nil {
		b.telegramClient.SendMessageToChat(ctx, chatID, fmt.Sprintf("❌ Failed to promote: %v", err))
		return
	}
	if !ok {
		b.telegramClient.SendMessageToChat(ctx, chatID, "❌ User not found.")
		return
	}

	targetChat := strconv.FormatInt(targetChatID, 10)
	_ = b.telegramClient.SendMessageToChat(ctx, targetChat, "✅ You have been promoted to admin.")
	b.telegramClient.SendMessageToChat(ctx, chatID, fmt.Sprintf("✅ Promoted chat_id %d to admin", targetChatID))
}

func parseChatIDArg(args string) (int64, error) {
	value := strings.TrimSpace(args)
	if value == "" {
		return 0, fmt.Errorf("missing")
	}
	chatID, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, err
	}
	return chatID, nil
}

// handlePing responds with a pong
func (b *Bot) handlePing(ctx context.Context, chatID string) {
	b.telegramClient.SendMessageToChat(ctx, chatID, "🏓 Pong! Bot is alive.\n\nType /help for commands.")
}

// handleLogout logs out the user
func (b *Bot) handleLogout(ctx context.Context, chatID string) {
	chatIDInt, _ := strconv.ParseInt(chatID, 10, 64)
	if err := b.db.ClearSession(ctx, chatIDInt); err != nil {
		log.Printf("[BOT] Error clearing session: %v", err)
		b.telegramClient.SendMessageToChat(ctx, chatID, "❌ Error logging out. Please try again.")
		return
	}
	b.telegramClient.SendMessageToChat(ctx, chatID, "👋 Logged out. Send /start and enter PIN to login again.")
}

// handleWhoami shows user profile
func (b *Bot) handleWhoami(ctx context.Context, chatID string) {
	chatIDInt, _ := strconv.ParseInt(chatID, 10, 64)
	user, err := b.db.GetUser(ctx, chatIDInt)
	if err != nil || user == nil {
		b.telegramClient.SendMessageToChat(ctx, chatID, "❓ Could not retrieve your profile.\n\nType /help for commands.")
		return
	}
	session, _ := b.db.GetSession(ctx, chatIDInt)
	sessionStatus := "Not logged in"
	if session != nil && session.ExpiresAt.After(time.Now()) {
		sessionStatus = fmt.Sprintf("Active (expires %s)", session.ExpiresAt.Format("2006-01-02 15:04"))
	}

	msg := fmt.Sprintf(`👤 <b>Your Profile</b>

📛 <b>Name:</b> %s
🆔 <b>Chat ID:</b> %d
🎖️ <b>Role:</b> %s
📌 <b>Status:</b> %s
🔑 <b>Session:</b> %s

Type /help for commands.`,
		user.Name,
		user.ChatID,
		user.Role,
		user.Status,
		sessionStatus)

	b.telegramClient.SendMessageToChat(ctx, chatID, msg)
}
