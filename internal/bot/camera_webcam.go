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
	"sort"
	"strconv"
	"strings"
	"time"

	"sentinel-go/internal/config"
	"sentinel-go/internal/imaging"
)

const unknownWebcamCommandMsg = "❌ Unknown webcam command.\n\nUse:\n• /cam\n• /cam gif\n• /cam list\n• /cam show [number]"

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
		b.telegramClient.SendMessageToChat(ctx, chatID, unknownWebcamCommandMsg)
		return
	default:
		b.telegramClient.SendMessageToChat(ctx, chatID, unknownWebcamCommandMsg)
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
