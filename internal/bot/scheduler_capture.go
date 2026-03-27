package bot

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"sentinel-go/internal/imaging"
)

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

// Stop gracefully stops the bot
func (b *Bot) Stop() {
	close(b.stopScheduler)
}
