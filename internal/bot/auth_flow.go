package bot

import (
	"context"
	"fmt"
	"html"
	"log"
	"strconv"
	"strings"
	"time"

	"sentinel-go/internal/database"
	"sentinel-go/internal/telegram"

	"golang.org/x/crypto/bcrypt"
)

func (b *Bot) canAccessCommand(role string, cmd *telegram.Command) bool {
	if role == "admin" {
		return true
	}

	switch cmd.Name {
	case "start", "help", "h", "capture", "snap", "c", "cam", "webcam",
		"interval", "int", "enable", "on", "disable", "off", "list", "cams",
		"cameras", "status", "scheduler", "sched", "ping", "whoami":
		return true
	default:
		return false
	}
}

func (b *Bot) requiresServicePIN(role string, cmd *telegram.Command) bool {
	if role != "admin" {
		return false
	}
	if cmd.Name == "start" && strings.TrimSpace(cmd.Args) != "" {
		return true
	}
	switch cmd.Name {
	case "stop", "restart", "res", "services", "svc", "logs", "sysinfo", "sys":
		return true
	default:
		return false
	}
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
