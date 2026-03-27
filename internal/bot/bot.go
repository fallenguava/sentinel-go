package bot

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"sentinel-go/internal/alerts"
	"sentinel-go/internal/cctv"
	"sentinel-go/internal/config"
	"sentinel-go/internal/database"
	"sentinel-go/internal/telegram"
	"sentinel-go/internal/webcam"
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
