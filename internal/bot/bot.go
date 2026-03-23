package bot

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"image"
	"image/color/palette"
	"image/draw"
	"image/gif"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"sentinel-go/internal/alerts"
	"sentinel-go/internal/cctv"
	"sentinel-go/internal/config"
	"sentinel-go/internal/database"
	"sentinel-go/internal/imaging"
	"sentinel-go/internal/servicectl"
	"sentinel-go/internal/sysmon"
	"sentinel-go/internal/telegram"
	"sentinel-go/internal/webcam"

	"golang.org/x/crypto/bcrypt"
)

const (
	flowAwaitRegistrationName = "await_registration_name"
	flowAwaitRegistrationPIN  = "await_registration_pin"
	flowAwaitLoginPIN         = "await_login_pin"
	flowAwaitServicePIN       = "await_service_pin"
)

var pinRegex = regexp.MustCompile(`^\d{6}$`)

type flowState struct {
	Stage string

	RegistrationName string
	PendingCommand   *telegram.Command

	LoginAttempts   int
	LoginLockedTill time.Time

	ServiceAttempts   int
	ServiceLockedTill time.Time
}

// Bot handles Telegram bot commands and scheduled captures
type Bot struct {
	cfg            *config.Config
	db             *database.DB
	cctvClient     *cctv.Client
	webcamClient   *webcam.Client
	telegramClient *telegram.Client

	// Scheduler state
	schedulerMu     sync.RWMutex
	schedulerActive bool
	intervalMinutes int
	enabledCameras  map[int]bool
	stopScheduler   chan struct{}

	// Update offset for polling
	updateOffset int

	alertManager *alerts.Manager
	flowMu       sync.Mutex
	flows        map[int64]*flowState
}

// NewBot creates a new bot instance
func NewBot(cfg *config.Config, db *database.DB, cctvClient *cctv.Client, webcamClient *webcam.Client, telegramClient *telegram.Client) *Bot {
	// Initialize all cameras as enabled by default
	enabledCams := make(map[int]bool)
	for i := 1; i <= cfg.NumCams; i++ {
		enabledCams[i] = true
	}

	return &Bot{
		cfg:             cfg,
		db:              db,
		cctvClient:      cctvClient,
		webcamClient:    webcamClient,
		telegramClient:  telegramClient,
		schedulerActive: true,
		intervalMinutes: cfg.CronIntervalMinutes,
		enabledCameras:  enabledCams,
		stopScheduler:   make(chan struct{}),
		alertManager:    alerts.NewManager(telegramClient, time.Duration(cfg.AlertIntervalSeconds)*time.Second),
		flows:           make(map[int64]*flowState),
	}
}

// Start begins the bot's operation (polling + scheduled captures)
func (b *Bot) Start(ctx context.Context) {
	// Start the scheduler in a goroutine
	go b.runScheduler(ctx)
	go b.alertManager.Start(ctx)

	// Start polling for commands
	go b.pollUpdates(ctx)

	log.Println("[BOT] Bot started, waiting for messages...")
}

// pollUpdates continuously polls for new Telegram messages
func (b *Bot) pollUpdates(ctx context.Context) {
	log.Println("[BOT] Starting Telegram polling...")

	for {
		select {
		case <-ctx.Done():
			log.Println("[BOT] Stopping Telegram polling...")
			return
		default:
			updates, err := b.telegramClient.GetUpdates(ctx, b.updateOffset, 30)
			if err != nil {
				log.Printf("[BOT] Error getting updates: %v", err)
				time.Sleep(5 * time.Second)
				continue
			}

			for _, update := range updates {
				b.updateOffset = update.UpdateID + 1
				b.handleUpdate(ctx, update)
			}
		}
	}
}

// handleUpdate processes a single Telegram update
func (b *Bot) handleUpdate(ctx context.Context, update telegram.Update) {
	if update.Message == nil || update.Message.Text == "" {
		return
	}

	chatID := update.Message.Chat.ID
	chatIDStr := strconv.FormatInt(chatID, 10)
	text := strings.TrimSpace(update.Message.Text)
	cmd := telegram.ParseCommand(text)
	user, err := b.db.GetUser(ctx, chatID)
	if err != nil {
		log.Printf("[BOT] Error reading user: %v", err)
		return
	}

	if cmd == nil {
		b.handleInteractiveInput(ctx, chatID, chatIDStr, text, user)
		return
	}

	b.handleCommandInput(ctx, chatID, chatIDStr, cmd, user)
}

func (b *Bot) handleCommandInput(ctx context.Context, chatID int64, chatIDStr string, cmd *telegram.Command, user *database.User) {
	flow := b.getFlow(chatID)

	if flow.Stage == flowAwaitRegistrationName || flow.Stage == flowAwaitRegistrationPIN || flow.Stage == flowAwaitLoginPIN || flow.Stage == flowAwaitServicePIN {
		if cmd.Name != "start" {
			b.telegramClient.SendMessageToChat(ctx, chatIDStr, "ℹ️ Finish the current authentication step first. Send your PIN or name as requested.")
			return
		}
	}

	if user == nil {
		if cmd.Name == "start" {
			flow.Stage = flowAwaitRegistrationName
			flow.RegistrationName = ""
			flow.PendingCommand = nil
			b.telegramClient.SendMessageToChat(ctx, chatIDStr, "👤 Welcome. Please enter your display name to register.")
			return
		}
		b.telegramClient.SendMessageToChat(ctx, chatIDStr, "🔒 You are not registered. Send /start to begin registration.")
		return
	}

	if user.Status == "pending" {
		b.telegramClient.SendMessageToChat(ctx, chatIDStr, "⏳ Your request has been submitted. Please wait for admin approval.")
		return
	}

	if user.Status == "rejected" {
		b.telegramClient.SendMessageToChat(ctx, chatIDStr, "❌ Your access request was rejected.")
		return
	}

	active, err := b.db.IsSessionActive(ctx, chatID)
	if err != nil {
		log.Printf("[BOT] Error checking session: %v", err)
		b.telegramClient.SendMessageToChat(ctx, chatIDStr, "❌ Failed to validate your session. Please try again.")
		return
	}

	if !active {
		if flow.LoginLockedTill.After(time.Now()) {
			remaining := int(time.Until(flow.LoginLockedTill).Minutes()) + 1
			b.telegramClient.SendMessageToChat(ctx, chatIDStr, fmt.Sprintf("🚫 Too many wrong PIN attempts. Try again in about %d minutes.", remaining))
			return
		}
		flow.Stage = flowAwaitLoginPIN
		flow.PendingCommand = nil
		b.telegramClient.SendMessageToChat(ctx, chatIDStr, "🔐 Enter your PIN:")
		return
	}

	if err := b.db.TouchSession(ctx, chatID); err != nil {
		log.Printf("[BOT] Error touching session: %v", err)
	}

	if !b.canAccessCommand(user.Role, cmd) {
		b.telegramClient.SendMessageToChat(ctx, chatIDStr, "⛔ You are not allowed to use this command.")
		return
	}

	if b.requiresServicePIN(user.Role, cmd) {
		confirmed, err := b.db.HasValidServicePINConfirm(ctx, chatID)
		if err != nil {
			log.Printf("[BOT] Error checking PIN confirmation: %v", err)
			b.telegramClient.SendMessageToChat(ctx, chatIDStr, "❌ Failed to verify admin PIN confirmation.")
			return
		}
		if !confirmed {
			if flow.ServiceLockedTill.After(time.Now()) {
				remaining := int(time.Until(flow.ServiceLockedTill).Minutes()) + 1
				b.telegramClient.SendMessageToChat(ctx, chatIDStr, fmt.Sprintf("🚫 Too many wrong PIN attempts for service control. Try again in about %d minutes.", remaining))
				return
			}
			flow.Stage = flowAwaitServicePIN
			flow.PendingCommand = cmd
			b.telegramClient.SendMessageToChat(ctx, chatIDStr, "🔐 Enter your PIN to confirm:")
			return
		}
	}

	b.executeCommand(ctx, chatID, chatIDStr, cmd, user)
}

func (b *Bot) handleInteractiveInput(ctx context.Context, chatID int64, chatIDStr, text string, user *database.User) {
	flow := b.getFlow(chatID)

	switch flow.Stage {
	case flowAwaitRegistrationName:
		name := strings.TrimSpace(text)
		if name == "" {
			b.telegramClient.SendMessageToChat(ctx, chatIDStr, "❌ Name cannot be empty. Please enter your display name.")
			return
		}
		flow.RegistrationName = name
		flow.Stage = flowAwaitRegistrationPIN
		b.telegramClient.SendMessageToChat(ctx, chatIDStr, "🔐 Set a 6-digit PIN:")
		return

	case flowAwaitRegistrationPIN:
		pin := strings.TrimSpace(text)
		if !pinRegex.MatchString(pin) {
			b.telegramClient.SendMessageToChat(ctx, chatIDStr, "❌ PIN must be exactly 6 digits.")
			return
		}

		hash, err := bcrypt.GenerateFromPassword([]byte(pin), bcrypt.DefaultCost)
		if err != nil {
			log.Printf("[BOT] Error hashing PIN: %v", err)
			b.telegramClient.SendMessageToChat(ctx, chatIDStr, "❌ Failed to save your PIN. Please try again.")
			return
		}

		role := "user"
		status := "pending"
		if chatID == b.cfg.AdminChatID {
			role = "admin"
			status = "approved"
		}

		requestedName := flow.RegistrationName
		err = b.db.UpsertUser(ctx, chatID, requestedName, string(hash), role, status)
		if err != nil {
			log.Printf("[BOT] Error saving registration: %v", err)
			b.telegramClient.SendMessageToChat(ctx, chatIDStr, "❌ Failed to submit registration. Please try again.")
			return
		}

		flow.Stage = ""
		flow.RegistrationName = ""

		if status == "approved" {
			b.telegramClient.SendMessageToChat(ctx, chatIDStr, "✅ Your access has been approved! Send /help to get started.")
			return
		}

		notify := fmt.Sprintf("⏳ New registration request from %s (chat_id: %d). Use /approve %d or /reject %d", html.EscapeString(requestedName), chatID, chatID, chatID)
		adminChat := strconv.FormatInt(b.cfg.AdminChatID, 10)
		if err := b.telegramClient.SendMessageToChat(ctx, adminChat, notify); err != nil {
			log.Printf("[BOT] Failed to notify admin: %v", err)
		}

		b.telegramClient.SendMessageToChat(ctx, chatIDStr, "⏳ Your request has been submitted. Please wait for admin approval.")
		return

	case flowAwaitLoginPIN:
		if user == nil || user.Status != "approved" {
			flow.Stage = ""
			b.telegramClient.SendMessageToChat(ctx, chatIDStr, "❌ Your account is not approved.")
			return
		}
		if flow.LoginLockedTill.After(time.Now()) {
			remaining := int(time.Until(flow.LoginLockedTill).Minutes()) + 1
			b.telegramClient.SendMessageToChat(ctx, chatIDStr, fmt.Sprintf("🚫 Too many wrong PIN attempts. Try again in about %d minutes.", remaining))
			return
		}

		if bcrypt.CompareHashAndPassword([]byte(user.PinHash), []byte(strings.TrimSpace(text))) != nil {
			flow.LoginAttempts++
			if flow.LoginAttempts >= 3 {
				flow.LoginLockedTill = time.Now().Add(10 * time.Minute)
				flow.LoginAttempts = 0
				b.telegramClient.SendMessageToChat(ctx, chatIDStr, "🚫 Too many wrong PIN attempts. Login is locked for 10 minutes.")
				return
			}
			remaining := 3 - flow.LoginAttempts
			b.telegramClient.SendMessageToChat(ctx, chatIDStr, fmt.Sprintf("❌ Wrong PIN. %d attempt(s) remaining.", remaining))
			return
		}

		if err := b.db.CreateOrRefreshSession(ctx, chatID); err != nil {
			log.Printf("[BOT] Error creating session: %v", err)
			b.telegramClient.SendMessageToChat(ctx, chatIDStr, "❌ Failed to create session. Please try again.")
			return
		}

		flow.Stage = ""
		flow.LoginAttempts = 0
		flow.PendingCommand = nil
		b.telegramClient.SendMessageToChat(ctx, chatIDStr, "✅ Login successful.")
		return

	case flowAwaitServicePIN:
		if user == nil || user.Status != "approved" {
			flow.Stage = ""
			b.telegramClient.SendMessageToChat(ctx, chatIDStr, "❌ Your account is not approved.")
			return
		}
		if flow.ServiceLockedTill.After(time.Now()) {
			remaining := int(time.Until(flow.ServiceLockedTill).Minutes()) + 1
			b.telegramClient.SendMessageToChat(ctx, chatIDStr, fmt.Sprintf("🚫 Too many wrong PIN attempts for service control. Try again in about %d minutes.", remaining))
			return
		}

		if bcrypt.CompareHashAndPassword([]byte(user.PinHash), []byte(strings.TrimSpace(text))) != nil {
			flow.ServiceAttempts++
			if flow.ServiceAttempts >= 3 {
				flow.ServiceLockedTill = time.Now().Add(10 * time.Minute)
				flow.ServiceAttempts = 0
				b.telegramClient.SendMessageToChat(ctx, chatIDStr, "🚫 Too many wrong PIN attempts. Service control is locked for 10 minutes.")
				return
			}
			remaining := 3 - flow.ServiceAttempts
			b.telegramClient.SendMessageToChat(ctx, chatIDStr, fmt.Sprintf("❌ Wrong PIN. %d attempt(s) remaining.", remaining))
			return
		}

		if err := b.db.SetPinConfirmed(ctx, chatID); err != nil {
			log.Printf("[BOT] Error setting PIN confirmation: %v", err)
			b.telegramClient.SendMessageToChat(ctx, chatIDStr, "❌ Failed to confirm PIN.")
			return
		}

		pending := flow.PendingCommand
		flow.Stage = ""
		flow.ServiceAttempts = 0
		flow.PendingCommand = nil

		b.telegramClient.SendMessageToChat(ctx, chatIDStr, "✅ PIN confirmed.")
		if pending != nil {
			b.executeCommand(ctx, chatID, chatIDStr, pending, user)
		}
		return
	}
}

func (b *Bot) executeCommand(ctx context.Context, chatID int64, chatIDStr string, cmd *telegram.Command, user *database.User) {
	log.Printf("[BOT] [%s] Command: /%s %s", chatIDStr, cmd.Name, cmd.Args)

	switch cmd.Name {
	case "help", "h":
		b.handleHelp(ctx, chatIDStr, user.Role)
	case "start":
		if strings.TrimSpace(cmd.Args) == "" {
			b.handleStart(ctx, chatIDStr)
		} else {
			b.handleServiceStart(ctx, chatIDStr, cmd.Args)
		}
	case "status":
		b.handleStatus(ctx, chatIDStr)
	case "capture", "snap", "c":
		b.handleCapture(ctx, chatIDStr, cmd.Args)
	case "cam", "webcam":
		b.handleCam(ctx, chatIDStr, cmd.Args)
	case "interval", "int":
		b.handleInterval(ctx, chatIDStr, cmd.Args)
	case "enable", "on":
		b.handleEnable(ctx, chatIDStr, cmd.Args)
	case "disable", "off":
		b.handleDisable(ctx, chatIDStr, cmd.Args)
	case "list", "cams", "cameras":
		b.handleList(ctx, chatIDStr)
	case "scheduler", "sched":
		b.handleScheduler(ctx, chatIDStr, cmd.Args)
	case "ping":
		b.handlePing(ctx, chatIDStr)
	case "whoami":
		b.handleWhoami(ctx, chatIDStr)
	case "sysinfo", "sys":
		b.handleSysInfo(ctx, chatIDStr)
	case "services", "svc":
		b.handleServices(ctx, chatIDStr)
	case "logs":
		b.handleLogs(ctx, chatIDStr, cmd.Args)
	case "stop":
		b.handleServiceStop(ctx, chatIDStr, cmd.Args)
	case "restart", "res":
		b.handleServiceRestart(ctx, chatIDStr, cmd.Args)
	case "approve":
		b.handleApprove(ctx, chatIDStr, cmd.Args)
	case "reject":
		b.handleReject(ctx, chatIDStr, cmd.Args)
	case "users":
		b.handleUsers(ctx, chatIDStr)
	case "revoke":
		b.handleRevoke(ctx, chatIDStr, cmd.Args)
	case "promote":
		b.handlePromote(ctx, chatIDStr, cmd.Args)
	default:
		msg := fmt.Sprintf("❓ Unknown command: /%s\n\nType /help for available commands.", cmd.Name)
		b.telegramClient.SendMessageToChat(ctx, chatIDStr, msg)
	}
}

func (b *Bot) canAccessCommand(role string, cmd *telegram.Command) bool {
	if role == "admin" {
		return true
	}

	allowed := map[string]bool{
		"start":     true,
		"help":      true,
		"h":         true,
		"capture":   true,
		"snap":      true,
		"c":         true,
		"cam":       true,
		"webcam":    true,
		"interval":  true,
		"int":       true,
		"enable":    true,
		"on":        true,
		"disable":   true,
		"off":       true,
		"list":      true,
		"cams":      true,
		"cameras":   true,
		"status":    true,
		"scheduler": true,
		"sched":     true,
		"ping":      true,
		"whoami":    true,
	}

	return allowed[cmd.Name]
}

func (b *Bot) requiresServicePIN(role string, cmd *telegram.Command) bool {
	if role != "admin" {
		return false
	}
	if cmd.Name == "start" && strings.TrimSpace(cmd.Args) != "" {
		return true
	}
	return cmd.Name == "stop" || cmd.Name == "restart" || cmd.Name == "res" || cmd.Name == "services" || cmd.Name == "svc" || cmd.Name == "logs" || cmd.Name == "sysinfo" || cmd.Name == "sys"
}

func (b *Bot) getFlow(chatID int64) *flowState {
	b.flowMu.Lock()
	defer b.flowMu.Unlock()

	state, ok := b.flows[chatID]
	if !ok {
		state = &flowState{}
		b.flows[chatID] = state
	}
	return state
}

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

// handleStatus sends the current status
func (b *Bot) handleStatus(ctx context.Context, chatID string) {
	b.schedulerMu.RLock()
	defer b.schedulerMu.RUnlock()

	schedulerStatus := "🔴 Stopped"
	if b.schedulerActive {
		schedulerStatus = "🟢 Active"
	}

	enabledCount := 0
	enabledList := []string{}
	for i := 1; i <= b.cfg.NumCams; i++ {
		if b.enabledCameras[i] {
			enabledCount++
			enabledList = append(enabledList, fmt.Sprintf("%d", i))
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
	if service == "" {
		b.telegramClient.SendMessageToChat(ctx, chatID,
			"Usage: <code>/logs [service]</code>\n\nAllowed services:\n<code>aurexa-fastify, sentinel-go, gitea, gitea-runner, postgresql, cloudflared, netdata</code>")
		return
	}

	if !servicectl.IsAllowedService(service) {
		b.telegramClient.SendMessageToChat(ctx, chatID,
			fmt.Sprintf("❌ Service not allowed: <b>%s</b>\n\nAllowed services:\n<code>%s</code>",
				html.EscapeString(service), strings.Join(servicectl.MonitoredServices(), ", ")))
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
	if service == "" {
		b.telegramClient.SendMessageToChat(ctx, chatID,
			fmt.Sprintf("Usage: <code>/%s [service]</code>\n\nAllowed services:\n<code>%s</code>",
				action, strings.Join(servicectl.MonitoredServices(), ", ")))
		return
	}

	if !servicectl.IsAllowedService(service) {
		b.telegramClient.SendMessageToChat(ctx, chatID,
			fmt.Sprintf("❌ Service not allowed: <b>%s</b>\n\nAllowed services:\n<code>%s</code>",
				html.EscapeString(service), strings.Join(servicectl.MonitoredServices(), ", ")))
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

// handleCapture captures snapshots from specified cameras
func (b *Bot) handleCapture(ctx context.Context, chatID string, args string) {
	var cameras []int
	var err error

	if args == "" {
		// Use enabled cameras only
		b.schedulerMu.RLock()
		for i := 1; i <= b.cfg.NumCams; i++ {
			if b.enabledCameras[i] {
				cameras = append(cameras, i)
			}
		}
		b.schedulerMu.RUnlock()

		if len(cameras) == 0 {
			b.telegramClient.SendMessageToChat(ctx, chatID,
				"⚠️ No cameras enabled. Use /enable to enable cameras.\n\nType /help for commands.")
			return
		}
	} else {
		cameras, err = config.ParseCameras(args, b.cfg.NumCams)
		if err != nil {
			b.telegramClient.SendMessageToChat(ctx, chatID,
				fmt.Sprintf("❌ Invalid camera selection: %v\n\nType /help for commands.", err))
			return
		}
	}

	b.telegramClient.SendMessageToChat(ctx, chatID,
		fmt.Sprintf("📷 Capturing from %d camera(s)...", len(cameras)))

	b.captureAndSend(ctx, chatID, cameras, false)
}

// handleCam supports webcam capture modes:
// - /cam or /webcam: 4-frame collage with interval
// - /cam gif: 4-frame animated GIF with short interval
// - /cam list: show last 20 captures
// - /cam show [n]: send selected capture from list
func (b *Bot) handleCam(ctx context.Context, chatID string, args string) {
	mode := strings.ToLower(strings.TrimSpace(args))

	switch {
	case mode == "":
		b.handleCamCollage(ctx, chatID)
	case mode == "gif":
		b.handleCamGIF(ctx, chatID)
	case mode == "list" || mode == "history":
		b.handleCamList(ctx, chatID)
	case strings.HasPrefix(mode, "show "):
		index, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(mode, "show ")))
		if err != nil || index <= 0 {
			b.telegramClient.SendMessageToChat(ctx, chatID, "❌ Usage: /cam show [number]\n\nType /cam list first.")
			return
		}
		b.handleCamShow(ctx, chatID, index)
	case mode != "":
		index, err := strconv.Atoi(mode)
		if err == nil && index > 0 {
			b.handleCamShow(ctx, chatID, index)
			return
		}
		b.telegramClient.SendMessageToChat(ctx, chatID,
			"❌ Unknown webcam command.\n\nUse:\n• /cam\n• /cam gif\n• /cam list\n• /cam show [number]")
		return
	default:
		b.telegramClient.SendMessageToChat(ctx, chatID,
			"❌ Unknown webcam command.\n\nUse:\n• /cam\n• /cam gif\n• /cam list\n• /cam show [number]")
	}
}

func (b *Bot) handleCamCollage(ctx context.Context, chatID string) {
	log.Printf("[BOT] [%s] Webcam: Starting 4-frame collage sequence", chatID)
	b.telegramClient.SendMessageToChat(ctx, chatID, "📷 Processing... Capturing 4 frames with 2s intervals")

	images, err := b.captureWebcamFrames(ctx, chatID, 4, 2*time.Second, 30*time.Second)
	if err != nil {
		b.telegramClient.SendMessageToChat(ctx, chatID,
			fmt.Sprintf("❌ Webcam capture failed: %v\n\nType /help for commands.", err))
		return
	}

	startTime := time.Now()
	b.telegramClient.SendMessageToChat(ctx, chatID, "🎨 Creating collage...")
	collageData, err := imaging.CreateCollage(images, imaging.DefaultCollageConfig())
	if err != nil {
		log.Printf("[BOT] [%s] Webcam: Collage creation failed: %v", chatID, err)
		b.telegramClient.SendMessageToChat(ctx, chatID,
			fmt.Sprintf("❌ Collage creation failed: %v\n\nType /help for commands.", err))
		return
	}

	if err := b.saveWebcamOutput("collage", ".jpg", collageData); err != nil {
		log.Printf("[BOT] [%s] Webcam: Failed to save collage history: %v", chatID, err)
	}

	caption := fmt.Sprintf("🖥️ Laptop Webcam - 4 Frame Collage\n🕐 %s", time.Now().Format("2006-01-02 15:04:05"))
	if err := b.telegramClient.SendPhotoToChat(ctx, chatID, collageData, "image/jpeg", caption); err != nil {
		log.Printf("[BOT] [%s] Webcam: Failed to send collage: %v", chatID, err)
		b.telegramClient.SendMessageToChat(ctx, chatID,
			fmt.Sprintf("❌ Failed to send collage: %v\n\nType /help for commands.", err))
		return
	}

	log.Printf("[BOT] [%s] Webcam: ✅ Collage sent successfully in %0.2fs", chatID, time.Since(startTime).Seconds())
}

func (b *Bot) handleCamGIF(ctx context.Context, chatID string) {
	log.Printf("[BOT] [%s] Webcam: Starting 4-frame GIF sequence", chatID)
	b.telegramClient.SendMessageToChat(ctx, chatID, "🎞️ Processing... Capturing 4 frames for GIF (0.5s intervals)")

	images, err := b.captureWebcamFrames(ctx, chatID, 4, 500*time.Millisecond, 20*time.Second)
	if err != nil {
		b.telegramClient.SendMessageToChat(ctx, chatID,
			fmt.Sprintf("❌ GIF capture failed: %v\n\nType /help for commands.", err))
		return
	}

	b.telegramClient.SendMessageToChat(ctx, chatID, "🎬 Building animated GIF...")
	gifData, err := createAnimatedGIF(images, 40)
	if err != nil {
		log.Printf("[BOT] [%s] Webcam: GIF creation failed: %v", chatID, err)
		b.telegramClient.SendMessageToChat(ctx, chatID,
			fmt.Sprintf("❌ GIF creation failed: %v\n\nType /help for commands.", err))
		return
	}

	if err := b.saveWebcamOutput("gif", ".gif", gifData); err != nil {
		log.Printf("[BOT] [%s] Webcam: Failed to save GIF history: %v", chatID, err)
	}

	caption := fmt.Sprintf("🎞️ Laptop Webcam GIF (4 frames)\n🕐 %s", time.Now().Format("2006-01-02 15:04:05"))
	if err := b.telegramClient.SendDocumentToChat(ctx, chatID, gifData, "webcam_capture.gif", caption); err != nil {
		log.Printf("[BOT] [%s] Webcam: Failed to send GIF: %v", chatID, err)
		b.telegramClient.SendMessageToChat(ctx, chatID,
			fmt.Sprintf("❌ Failed to send GIF: %v\n\nType /help for commands.", err))
		return
	}

	log.Printf("[BOT] [%s] Webcam: ✅ GIF sent successfully", chatID)
}

func (b *Bot) captureWebcamFrames(ctx context.Context, chatID string, frameCount int, interval time.Duration, timeout time.Duration) ([]*imaging.CapturedImage, error) {
	var images []*imaging.CapturedImage

	for frameNum := 1; frameNum <= frameCount; frameNum++ {
		log.Printf("[BOT] [%s] Webcam: Capturing frame %d/%d", chatID, frameNum, frameCount)

		captureCtx, cancel := context.WithTimeout(ctx, timeout)
		imageData, contentType, err := b.webcamClient.CaptureFrame(captureCtx)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("frame %d capture failed: %w", frameNum, err)
		}

		img, _, err := image.Decode(bytes.NewReader(imageData))
		if err != nil {
			return nil, fmt.Errorf("frame %d decode failed: %w", frameNum, err)
		}

		images = append(images, &imaging.CapturedImage{
			CamNumber: frameNum,
			CamName:   fmt.Sprintf("Frame %d", frameNum),
			Data:      imageData,
			Image:     img,
		})

		log.Printf("[BOT] [%s] Webcam: Frame %d captured (%d bytes, %s)", chatID, frameNum, len(imageData), contentType)

		if frameNum < frameCount {
			select {
			case <-time.After(interval):
			case <-ctx.Done():
				return nil, fmt.Errorf("capture interrupted")
			}
		}
	}

	return images, nil
}

func createAnimatedGIF(images []*imaging.CapturedImage, delay int) ([]byte, error) {
	if len(images) == 0 {
		return nil, fmt.Errorf("no frames to encode")
	}

	out := &gif.GIF{}
	for _, frame := range images {
		bounds := frame.Image.Bounds()
		paletted := image.NewPaletted(bounds, palette.Plan9)
		draw.FloydSteinberg.Draw(paletted, bounds, frame.Image, image.Point{})
		out.Image = append(out.Image, paletted)
		out.Delay = append(out.Delay, delay)
	}

	buf := &bytes.Buffer{}
	if err := gif.EncodeAll(buf, out); err != nil {
		return nil, fmt.Errorf("failed to encode gif: %w", err)
	}
	return buf.Bytes(), nil
}

type webcamHistoryItem struct {
	Name    string
	Path    string
	ModTime time.Time
	Size    int64
}

func (b *Bot) handleCamList(ctx context.Context, chatID string) {
	items, err := b.getWebcamHistory(20)
	if err != nil {
		b.telegramClient.SendMessageToChat(ctx, chatID, fmt.Sprintf("❌ Failed to read webcam history: %v", err))
		return
	}
	if len(items) == 0 {
		b.telegramClient.SendMessageToChat(ctx, chatID, "ℹ️ No webcam captures found yet.")
		return
	}

	lines := []string{"🗂️ <b>Last 20 Webcam Captures</b>", ""}
	for i, item := range items {
		lines = append(lines, fmt.Sprintf("%d. %s (%s)", i+1, html.EscapeString(item.Name), item.ModTime.Format("2006-01-02 15:04:05")))
	}
	lines = append(lines, "", "Use <code>/cam show [number]</code> to view one capture.")
	b.telegramClient.SendMessageToChat(ctx, chatID, strings.Join(lines, "\n"))
}

func (b *Bot) handleCamShow(ctx context.Context, chatID string, index int) {
	items, err := b.getWebcamHistory(20)
	if err != nil {
		b.telegramClient.SendMessageToChat(ctx, chatID, fmt.Sprintf("❌ Failed to read webcam history: %v", err))
		return
	}
	if index < 1 || index > len(items) {
		b.telegramClient.SendMessageToChat(ctx, chatID, fmt.Sprintf("❌ Invalid capture number. Choose 1-%d.", len(items)))
		return
	}

	item := items[index-1]
	data, err := os.ReadFile(item.Path)
	if err != nil {
		b.telegramClient.SendMessageToChat(ctx, chatID, fmt.Sprintf("❌ Failed to read capture: %v", err))
		return
	}

	caption := fmt.Sprintf("🗂️ Webcam Capture #%d\n🕐 %s\n📄 %s", index, item.ModTime.Format("2006-01-02 15:04:05"), item.Name)
	if strings.HasSuffix(strings.ToLower(item.Name), ".gif") {
		if err := b.telegramClient.SendDocumentToChat(ctx, chatID, data, item.Name, caption); err != nil {
			b.telegramClient.SendMessageToChat(ctx, chatID, fmt.Sprintf("❌ Failed to send capture: %v", err))
			return
		}
		return
	}

	if err := b.telegramClient.SendPhotoToChat(ctx, chatID, data, "image/jpeg", caption); err != nil {
		b.telegramClient.SendMessageToChat(ctx, chatID, fmt.Sprintf("❌ Failed to send capture: %v", err))
	}
}

func (b *Bot) getWebcamHistory(limit int) ([]webcamHistoryItem, error) {
	dir := winPathToWSLPath(b.cfg.WebcamCaptureDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to read capture directory %s: %w", dir, err)
	}

	items := make([]webcamHistoryItem, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		nameLower := strings.ToLower(entry.Name())
		if !strings.HasSuffix(nameLower, ".jpg") && !strings.HasSuffix(nameLower, ".jpeg") && !strings.HasSuffix(nameLower, ".gif") {
			continue
		}

		fullPath := filepath.Join(dir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}

		items = append(items, webcamHistoryItem{
			Name:    entry.Name(),
			Path:    fullPath,
			ModTime: info.ModTime(),
			Size:    info.Size(),
		})
	}

	sort.Slice(items, func(i, j int) bool {
		return items[i].ModTime.After(items[j].ModTime)
	})

	if len(items) > limit {
		items = items[:limit]
	}

	return items, nil
}

func (b *Bot) saveWebcamOutput(kind, ext string, data []byte) error {
	timestamp := time.Now().Format("20060102_150405")
	name := fmt.Sprintf("sentinel_cam_%s_%s_%s%s", kind, timestamp, generateShortID(), ext)
	winPath := b.cfg.WebcamCaptureDir + "\\" + name
	wslPath := winPathToWSLPath(winPath)

	if err := os.WriteFile(wslPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write %s: %w", wslPath, err)
	}
	log.Printf("[BOT] Webcam: Saved %s output to %s", kind, wslPath)
	return nil
}

func generateShortID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano()%1000000)
}

func winPathToWSLPath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	if len(p) >= 2 && p[1] == ':' {
		drive := strings.ToLower(string(p[0]))
		p = fmt.Sprintf("/mnt/%s%s", drive, p[2:])
	}
	return p
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
		camStrs[i] = fmt.Sprintf("%d", cam)
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
		camStrs[i] = fmt.Sprintf("%d", cam)
	}

	b.telegramClient.SendMessageToChat(ctx, chatID,
		fmt.Sprintf("🚫 Disabled camera(s): <b>%s</b>\n\nType /help for commands.", strings.Join(camStrs, ", ")))
}

// handleList shows all cameras and their status
func (b *Bot) handleList(ctx context.Context, chatID string) {
	b.schedulerMu.RLock()
	defer b.schedulerMu.RUnlock()

	var lines []string
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

// runScheduler runs the scheduled capture loop
func (b *Bot) runScheduler(ctx context.Context) {
	for {
		b.schedulerMu.RLock()
		active := b.schedulerActive
		interval := b.intervalMinutes
		b.schedulerMu.RUnlock()

		if !active {
			select {
			case <-ctx.Done():
				return
			case <-b.stopScheduler:
				continue
			case <-time.After(1 * time.Second):
				continue
			}
		}

		log.Printf("[SCHEDULER] Next capture in %d minutes", interval)

		select {
		case <-ctx.Done():
			return
		case <-b.stopScheduler:
			continue
		case <-time.After(time.Duration(interval) * time.Minute):
			b.schedulerMu.RLock()
			if !b.schedulerActive {
				b.schedulerMu.RUnlock()
				continue
			}

			var cameras []int
			for i := 1; i <= b.cfg.NumCams; i++ {
				if b.enabledCameras[i] {
					cameras = append(cameras, i)
				}
			}
			b.schedulerMu.RUnlock()

			if len(cameras) > 0 {
				log.Printf("[SCHEDULER] Running scheduled capture for %d cameras", len(cameras))
				// Send to all authorized users
				b.sendScheduledCapture(ctx, cameras)
			}
		}
	}
}

// sendScheduledCapture sends scheduled captures to all authorized users
func (b *Bot) sendScheduledCapture(ctx context.Context, cameras []int) {
	users, err := b.db.ListApprovedUsers(ctx)
	if err != nil {
		log.Printf("[SCHEDULER] Error getting authorized users: %v", err)
		return
	}

	if len(users) == 0 {
		log.Println("[SCHEDULER] No authorized users to send to")
		return
	}

	// Capture once, send to all
	captureCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	var capturedImages []*imaging.CapturedImage
	for _, cam := range cameras {
		imageData, _, err := b.cctvClient.CaptureSnapshot(captureCtx, cam)
		if err != nil {
			log.Printf("[SCHEDULER] Failed to capture Camera %d: %v", cam, err)
			continue
		}

		capturedImages = append(capturedImages, &imaging.CapturedImage{
			CamNumber: cam,
			CamName:   b.cfg.GetCameraName(cam),
			Data:      imageData,
		})
	}

	if len(capturedImages) == 0 {
		return
	}

	// Create collage
	var imageData []byte
	var caption string

	if len(capturedImages) == 1 {
		imageData = capturedImages[0].Data
		caption = fmt.Sprintf("🔄 📷 %s\n🕐 %s",
			capturedImages[0].CamName,
			time.Now().Format("2006-01-02 15:04:05"))
	} else {
		collageConfig := imaging.HighQualityCollageConfig()
		collageData, err := imaging.CreateCollage(capturedImages, collageConfig)
		if err != nil {
			log.Printf("[SCHEDULER] Failed to create collage: %v", err)
			return
		}
		imageData = collageData

		camList := make([]string, len(capturedImages))
		for i, img := range capturedImages {
			camList[i] = fmt.Sprintf("%d", img.CamNumber)
		}
		caption = fmt.Sprintf("🔄 📷 Cameras: %s\n🕐 %s",
			strings.Join(camList, ", "),
			time.Now().Format("2006-01-02 15:04:05"))
	}

	// Send to all authorized users
	for _, user := range users {
		chatIDStr := strconv.FormatInt(user.ChatID, 10)
		if err := b.telegramClient.SendPhotoToChat(captureCtx, chatIDStr, imageData, "image/jpeg", caption); err != nil {
			log.Printf("[SCHEDULER] Failed to send to %s (chat %d): %v", user.Name, user.ChatID, err)
		}
	}

	log.Printf("[SCHEDULER] Sent to %d authorized users", len(users))
}

// restartScheduler signals the scheduler to restart with new settings
func (b *Bot) restartScheduler() {
	select {
	case b.stopScheduler <- struct{}{}:
	default:
	}
}

// captureAndSend captures from specified cameras and sends to Telegram as a collage
func (b *Bot) captureAndSend(ctx context.Context, chatID string, cameras []int, isScheduled bool) {
	captureCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	var capturedImages []*imaging.CapturedImage
	failedCams := []int{}

	log.Printf("[CAPTURE] Starting capture for %d cameras", len(cameras))

	for _, cam := range cameras {
		imageData, _, err := b.cctvClient.CaptureSnapshot(captureCtx, cam)
		if err != nil {
			log.Printf("[CAPTURE] Failed to capture Camera %d: %v", cam, err)
			failedCams = append(failedCams, cam)
			continue
		}

		capturedImages = append(capturedImages, &imaging.CapturedImage{
			CamNumber: cam,
			CamName:   b.cfg.GetCameraName(cam),
			Data:      imageData,
		})
	}

	if len(capturedImages) == 0 {
		b.telegramClient.SendMessageToChat(ctx, chatID,
			fmt.Sprintf("❌ Failed to capture any cameras\nFailed: %v\n\nType /help for commands.", failedCams))
		return
	}

	if len(capturedImages) == 1 {
		img := capturedImages[0]
		scheduleTag := ""
		if isScheduled {
			scheduleTag = "🔄 "
		}
		caption := fmt.Sprintf("%s📷 %s\n🕐 %s",
			scheduleTag,
			img.CamName,
			time.Now().Format("2006-01-02 15:04:05"))

		if err := b.telegramClient.SendPhotoToChat(captureCtx, chatID, img.Data, "image/jpeg", caption); err != nil {
			log.Printf("[CAPTURE] Failed to send photo: %v", err)
			b.telegramClient.SendMessageToChat(ctx, chatID, "❌ Failed to send photo\n\nType /help for commands.")
		}
		return
	}

	log.Printf("[CAPTURE] Creating collage from %d images...", len(capturedImages))

	collageConfig := imaging.HighQualityCollageConfig()
	collageData, err := imaging.CreateCollage(capturedImages, collageConfig)
	if err != nil {
		log.Printf("[CAPTURE] Failed to create collage: %v", err)
		b.telegramClient.SendMessageToChat(ctx, chatID,
			fmt.Sprintf("❌ Failed to create collage: %v\n\nType /help for commands.", err))
		return
	}

	scheduleTag := ""
	if isScheduled {
		scheduleTag = "🔄 Scheduled "
	}

	camList := make([]string, len(capturedImages))
	for i, img := range capturedImages {
		camList[i] = fmt.Sprintf("%d", img.CamNumber)
	}

	caption := fmt.Sprintf("%s📷 Cameras: %s\n🕐 %s",
		scheduleTag,
		strings.Join(camList, ", "),
		time.Now().Format("2006-01-02 15:04:05"))

	if len(failedCams) > 0 {
		failedStr := make([]string, len(failedCams))
		for i, c := range failedCams {
			failedStr[i] = fmt.Sprintf("%d", c)
		}
		caption += fmt.Sprintf("\n⚠️ Failed: %s", strings.Join(failedStr, ", "))
	}

	if err := b.telegramClient.SendPhotoToChat(captureCtx, chatID, collageData, "image/jpeg", caption); err != nil {
		log.Printf("[CAPTURE] Failed to send collage: %v", err)
		b.telegramClient.SendMessageToChat(ctx, chatID,
			fmt.Sprintf("❌ Failed to send collage: %v\n\nType /help for commands.", err))
		return
	}

	log.Printf("[CAPTURE] Successfully sent collage with %d cameras", len(capturedImages))
}

// sendPraisingImage sends the success/renewal message with Win's photo
func (b *Bot) sendPraisingImage(ctx context.Context, chatID string, caption string) {
	// Try to read the praising image
	imageData, err := os.ReadFile("assets/win.jpg")
	if err != nil {
		log.Printf("[BOT] Could not read praising image: %v", err)
		// Fall back to text-only message
		b.telegramClient.SendMessageToChat(ctx, chatID, caption)
		return
	}

	// Send photo with caption
	if err := b.telegramClient.SendPhotoToChat(ctx, chatID, imageData, "image/jpeg", caption); err != nil {
		log.Printf("[BOT] Failed to send praising image: %v", err)
		// Fall back to text-only message
		b.telegramClient.SendMessageToChat(ctx, chatID, caption)
	}
}

// Stop gracefully stops the bot
func (b *Bot) Stop() {
	close(b.stopScheduler)
}
