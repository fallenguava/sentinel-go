package bot

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"sentinel-go/internal/auth"
	"sentinel-go/internal/cctv"
	"sentinel-go/internal/config"
	"sentinel-go/internal/database"
	"sentinel-go/internal/imaging"
	"sentinel-go/internal/telegram"
)

// Bot handles Telegram bot commands and scheduled captures
type Bot struct {
	cfg            *config.Config
	db             *database.DB
	cctvClient     *cctv.Client
	telegramClient *telegram.Client

	// Scheduler state
	schedulerMu     sync.RWMutex
	schedulerActive bool
	intervalMinutes int
	enabledCameras  map[int]bool
	stopScheduler   chan struct{}

	// Update offset for polling
	updateOffset int
}

// NewBot creates a new bot instance
func NewBot(cfg *config.Config, db *database.DB, cctvClient *cctv.Client, telegramClient *telegram.Client) *Bot {
	// Initialize all cameras as enabled by default
	enabledCams := make(map[int]bool)
	for i := 1; i <= cfg.NumCams; i++ {
		enabledCams[i] = true
	}

	return &Bot{
		cfg:             cfg,
		db:              db,
		cctvClient:      cctvClient,
		telegramClient:  telegramClient,
		schedulerActive: true,
		intervalMinutes: cfg.CronIntervalMinutes,
		enabledCameras:  enabledCams,
		stopScheduler:   make(chan struct{}),
	}
}

// Start begins the bot's operation (polling + scheduled captures)
func (b *Bot) Start(ctx context.Context) {
	// Start the scheduler in a goroutine
	go b.runScheduler(ctx)

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

	// Check if user is authorized
	authorized, err := b.db.IsAuthorized(ctx, chatID)
	if err != nil {
		log.Printf("[BOT] Error checking authorization: %v", err)
		return
	}

	// If authorized, handle commands normally
	if authorized {
		// Update last seen
		b.db.UpdateLastSeen(ctx, chatID)

		cmd := telegram.ParseCommand(text)
		if cmd == nil {
			// Not a command, ignore
			return
		}

		log.Printf("[BOT] [%s] Command: /%s %s", chatIDStr, cmd.Name, cmd.Args)
		b.handleCommand(ctx, chatIDStr, cmd)
		return
	}

	// Check if user exists but is expired (needs renewal)
	expired, _, err := b.db.IsExpired(ctx, chatID)
	if err != nil {
		log.Printf("[BOT] Error checking expiration: %v", err)
		return
	}

	if expired {
		// User exists but authorization expired - handle renewal
		b.handleRenewal(ctx, chatID, chatIDStr, text)
		return
	}

	// Not authorized - handle authentication flow (new user)
	b.handleAuth(ctx, chatID, chatIDStr, text)
}

// handleAuth handles the authentication flow
func (b *Bot) handleAuth(ctx context.Context, chatID int64, chatIDStr, text string) {
	// Get current auth state
	state, err := b.db.GetAuthState(ctx, chatID)
	if err != nil {
		log.Printf("[BOT] Error getting auth state: %v", err)
		return
	}

	// Check if blocked (too many attempts)
	if state.Attempts >= 3 {
		// Check if 10 minutes have passed
		if time.Since(state.UpdatedAt) < 10*time.Minute {
			b.telegramClient.SendMessageToChat(ctx, chatIDStr, auth.GetBlockedMessage())
			return
		}
		// Reset attempts after cooldown
		state.Attempts = 0
		state.Step = 0
	}

	// Handle /start command to begin auth
	if strings.HasPrefix(strings.ToLower(text), "/start") {
		state.Step = 1
		state.Attempts = 0
		b.db.UpdateAuthState(ctx, state)
		b.telegramClient.SendMessageToChat(ctx, chatIDStr, auth.GetWelcomeMessage())
		return
	}

	// State machine for authentication
	switch state.Step {
	case 0:
		// Not started, prompt to start
		b.telegramClient.SendMessageToChat(ctx, chatIDStr, auth.GetUnauthorizedMessage())

	case 1:
		// Waiting for name
		if auth.IsValidName(text) {
			state.Name = auth.NormalizeName(text)
			state.Step = 2
			b.db.UpdateAuthState(ctx, state)
			b.telegramClient.SendMessageToChat(ctx, chatIDStr, auth.GetSecurityQuestionMessage(state.Name))
		} else {
			state.Attempts++
			b.db.UpdateAuthState(ctx, state)
			b.telegramClient.SendMessageToChat(ctx, chatIDStr, auth.GetInvalidNameMessage())
		}

	case 2:
		// Waiting for security answer
		if auth.IsValidBirthdayMonth(text) {
			// Success! Add to authorized users
			if err := b.db.AddAuthorizedUser(ctx, chatID, state.Name); err != nil {
				log.Printf("[BOT] Error adding authorized user: %v", err)
				b.telegramClient.SendMessageToChat(ctx, chatIDStr, "❌ An error occurred. Please try again.")
				return
			}
			// Reset auth state
			b.db.ResetAuthState(ctx, chatID)
			// Send success message with praising image
			b.sendPraisingImage(ctx, chatIDStr, auth.GetSuccessMessage(state.Name))
			log.Printf("[BOT] New user authorized: %s (chat %d)", state.Name, chatID)
		} else {
			state.Attempts++
			b.db.UpdateAuthState(ctx, state)

			if state.Attempts >= 3 {
				b.telegramClient.SendMessageToChat(ctx, chatIDStr, auth.GetBlockedMessage())
			} else {
				b.telegramClient.SendMessageToChat(ctx, chatIDStr, auth.GetInvalidAnswerMessage(state.Attempts))
			}
		}
	}
}

// handleRenewal handles the authorization renewal flow for expired users
func (b *Bot) handleRenewal(ctx context.Context, chatID int64, chatIDStr, text string) {
	// Get existing user info
	user, err := b.db.GetAuthorizedUser(ctx, chatID)
	if err != nil || user == nil {
		// User doesn't exist, redirect to normal auth
		b.handleAuth(ctx, chatID, chatIDStr, text)
		return
	}

	// Get current auth state
	state, err := b.db.GetAuthState(ctx, chatID)
	if err != nil {
		log.Printf("[BOT] Error getting auth state: %v", err)
		return
	}

	// Check if blocked (too many attempts)
	if state.Attempts >= 3 {
		if time.Since(state.UpdatedAt) < 10*time.Minute {
			b.telegramClient.SendMessageToChat(ctx, chatIDStr, auth.GetBlockedMessage())
			return
		}
		// Reset attempts after cooldown
		state.Attempts = 0
		state.Step = 0
	}

	// If not in renewal state (step 10), start renewal
	if state.Step != 10 {
		// Any message triggers renewal prompt
		state.Step = 10
		state.Name = user.Name // Preserve the name
		b.db.UpdateAuthState(ctx, state)
		b.telegramClient.SendMessageToChat(ctx, chatIDStr, auth.GetExpiredMessage(user.Name))
		return
	}

	// Step 10: Waiting for security answer for renewal
	if auth.IsValidBirthdayMonth(text) {
		// Success! Renew authorization
		if err := b.db.RenewAuthorization(ctx, chatID); err != nil {
			log.Printf("[BOT] Error renewing authorization: %v", err)
			b.telegramClient.SendMessageToChat(ctx, chatIDStr, "❌ An error occurred. Please try again.")
			return
		}
		// Reset auth state
		b.db.ResetAuthState(ctx, chatID)
		// Send renewed message with praising image
		b.sendPraisingImage(ctx, chatIDStr, auth.GetRenewedMessage(user.Name))
		log.Printf("[BOT] User authorization renewed: %s (chat %d)", user.Name, chatID)
	} else {
		state.Attempts++
		b.db.UpdateAuthState(ctx, state)

		if state.Attempts >= 3 {
			b.telegramClient.SendMessageToChat(ctx, chatIDStr, auth.GetBlockedMessage())
		} else {
			b.telegramClient.SendMessageToChat(ctx, chatIDStr, auth.GetInvalidAnswerMessage(state.Attempts))
		}
	}
}

// handleCommand routes commands to their handlers
func (b *Bot) handleCommand(ctx context.Context, chatID string, cmd *telegram.Command) {
	switch cmd.Name {
	case "help", "h":
		b.handleHelp(ctx, chatID)
	case "start":
		b.handleStart(ctx, chatID)
	case "status":
		b.handleStatus(ctx, chatID)
	case "capture", "snap", "c":
		b.handleCapture(ctx, chatID, cmd.Args)
	case "interval", "int":
		b.handleInterval(ctx, chatID, cmd.Args)
	case "enable", "on":
		b.handleEnable(ctx, chatID, cmd.Args)
	case "disable", "off":
		b.handleDisable(ctx, chatID, cmd.Args)
	case "list", "cams", "cameras":
		b.handleList(ctx, chatID)
	case "scheduler", "sched":
		b.handleScheduler(ctx, chatID, cmd.Args)
	case "ping":
		b.handlePing(ctx, chatID)
	case "logout":
		b.handleLogout(ctx, chatID)
	case "whoami":
		b.handleWhoami(ctx, chatID)
	default:
		msg := fmt.Sprintf("❓ Unknown command: /%s\n\nType /help for available commands.", cmd.Name)
		b.telegramClient.SendMessageToChat(ctx, chatID, msg)
	}
}

// handleStart sends welcome message for authenticated users
func (b *Bot) handleStart(ctx context.Context, chatID string) {
	chatIDInt, _ := strconv.ParseInt(chatID, 10, 64)
	user, _ := b.db.GetAuthorizedUser(ctx, chatIDInt)

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
func (b *Bot) handleHelp(ctx context.Context, chatID string) {
	help := `📋 <b>Sentinel-GO Commands</b>

<b>📷 Capture:</b>
/capture [cams] - Capture snapshot(s)
  <code>/capture</code> - All enabled cameras
  <code>/capture 1</code> - Camera 1 only
  <code>/capture 1,3,5</code> - Cameras 1, 3, 5
  <code>/capture 1-4</code> - Cameras 1 to 4
  <code>/capture all</code> - All cameras
  <i>Shortcuts: /c, /snap</i>

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

<b>🔄 Scheduler:</b>
/scheduler [on/off] - Control auto-capture
  <i>Shortcut: /sched</i>

<b>🔐 Account:</b>
/logout - Revoke your access

Type /help anytime to see this menu.`

	b.telegramClient.SendMessageToChat(ctx, chatID, help)
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

	if err := b.db.RemoveAuthorizedUser(ctx, chatIDInt); err != nil {
		log.Printf("[BOT] Error removing user: %v", err)
		b.telegramClient.SendMessageToChat(ctx, chatID, "❌ Error logging out. Please try again.")
		return
	}

	b.db.ResetAuthState(ctx, chatIDInt)
	b.telegramClient.SendMessageToChat(ctx, chatID, auth.GetLogoutMessage())
	log.Printf("[BOT] User logged out: chat %d", chatIDInt)
}

// handleWhoami shows user profile
func (b *Bot) handleWhoami(ctx context.Context, chatID string) {
	chatIDInt, _ := strconv.ParseInt(chatID, 10, 64)
	user, err := b.db.GetAuthorizedUser(ctx, chatIDInt)
	if err != nil || user == nil {
		b.telegramClient.SendMessageToChat(ctx, chatID, "❓ Could not retrieve your profile.\n\nType /help for commands.")
		return
	}

	// Calculate days remaining
	daysRemaining := int(time.Until(user.ExpiresAt).Hours() / 24)
	expiryStatus := fmt.Sprintf("%s (%d days left)", user.ExpiresAt.Format("2006-01-02"), daysRemaining)
	if daysRemaining <= 1 {
		expiryStatus = fmt.Sprintf("⚠️ %s (expiring soon!)", user.ExpiresAt.Format("2006-01-02 15:04"))
	}

	msg := fmt.Sprintf(`👤 <b>Your Profile</b>

📛 <b>Name:</b> %s
🆔 <b>Chat ID:</b> %d
📅 <b>Authorized:</b> %s
⏰ <b>Expires:</b> %s
👁️ <b>Last Active:</b> %s

Type /help for commands.`,
		user.Name,
		user.ChatID,
		user.CreatedAt.Format("2006-01-02 15:04"),
		expiryStatus,
		user.LastSeen.Format("2006-01-02 15:04"))

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
	users, err := b.db.GetAllAuthorizedUsers(ctx)
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
