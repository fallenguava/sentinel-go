package config

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

// Config holds all configuration values for the application
type Config struct {
	// DVR Configuration
	DVRIP   string
	DVRPort string
	DVRUser string
	DVRPass string
	NumCams int

	// Telegram Configuration
	TelegramBotToken string
	TelegramChatID   string
	AdminChatID      int64

	// Database Configuration
	DBHost string
	DBPort string
	DBUser string
	DBPass string
	DBName string

	// Scheduler Configuration
	CronIntervalMinutes  int
	AlertIntervalSeconds int

	// Webcam Configuration
	WebcamDeviceName string // dshow device name, e.g. "Integrated Webcam"
	WebcamFFmpegPath string // Windows path to ffmpeg.exe
	WebcamCaptureDir string // Windows path to capture directory, e.g. F:\SentinelGo\webcam_captures

	// Camera names (optional, for display purposes)
	CameraNames map[int]string
}

// Load reads configuration from .env file and environment variables
func Load() (*Config, error) {
	// Load .env file if it exists (ignore error if file doesn't exist)
	if err := godotenv.Load(); err != nil {
		log.Println("[CONFIG] Warning: .env file not found, using environment variables")
	}

	cfg := &Config{
		CameraNames: make(map[int]string),
	}

	// DVR Configuration
	cfg.DVRIP = getEnv("DVR_IP", "192.168.1.2")
	cfg.DVRPort = getEnv("DVR_PORT", "80")
	cfg.DVRUser = getEnv("DVR_USER", "")
	cfg.DVRPass = getEnv("DVR_PASS", "")

	// Number of cameras
	numCams, err := strconv.Atoi(getEnv("DVR_NUM_CAMS", "7"))
	if err != nil || numCams <= 0 {
		numCams = 7
	}
	cfg.NumCams = numCams

	// Telegram Configuration
	cfg.TelegramBotToken = getEnv("TELEGRAM_BOT_TOKEN", "")
	cfg.TelegramChatID = getEnv("TELEGRAM_CHAT_ID", "")
	adminChatID, err := strconv.ParseInt(getEnv("ADMIN_CHAT_ID", "0"), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid ADMIN_CHAT_ID: %w", err)
	}
	cfg.AdminChatID = adminChatID

	// Database Configuration
	cfg.DBHost = getEnv("DB_HOST", "localhost")
	cfg.DBPort = getEnv("DB_PORT", "5432")
	cfg.DBUser = getEnv("DB_USER", "postgres")
	cfg.DBPass = getEnv("DB_PASS", "postgres")
	cfg.DBName = getEnv("DB_NAME", "sentinel")

	// Scheduler Configuration
	cronInterval, err := strconv.Atoi(getEnv("CRON_INTERVAL_MINUTES", "60"))
	if err != nil {
		return nil, fmt.Errorf("invalid CRON_INTERVAL_MINUTES: %w", err)
	}
	cfg.CronIntervalMinutes = cronInterval

	alertInterval, err := strconv.Atoi(getEnv("ALERT_INTERVAL_SECONDS", "60"))
	if err != nil {
		return nil, fmt.Errorf("invalid ALERT_INTERVAL_SECONDS: %w", err)
	}
	cfg.AlertIntervalSeconds = alertInterval

	// Webcam Configuration (optional)
	cfg.WebcamDeviceName = getEnv("WEBCAM_DEVICE_NAME", "Integrated Webcam")
	cfg.WebcamFFmpegPath = getEnv("WEBCAM_FFMPEG_PATH", `C:\tools\ffmpeg\bin\ffmpeg.exe`)
	cfg.WebcamCaptureDir = getEnv("WEBCAM_CAPTURE_DIR", `F:\SentinelGo\webcam_captures`)

	// Load camera names (optional)
	// Format: CAM_1_NAME=Front Door, CAM_2_NAME=Backyard, etc.
	for i := 1; i <= cfg.NumCams; i++ {
		name := getEnv(fmt.Sprintf("CAM_%d_NAME", i), fmt.Sprintf("Camera %d", i))
		cfg.CameraNames[i] = name
	}

	// Validate required fields
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	log.Println("[CONFIG] Configuration loaded successfully")
	return cfg, nil
}

// validate checks that all required configuration values are present
func (c *Config) validate() error {
	if c.DVRUser == "" {
		return fmt.Errorf("DVR_USER is required")
	}
	if c.DVRPass == "" {
		return fmt.Errorf("DVR_PASS is required")
	}
	if c.TelegramBotToken == "" {
		return fmt.Errorf("TELEGRAM_BOT_TOKEN is required")
	}
	if c.TelegramChatID == "" {
		return fmt.Errorf("TELEGRAM_CHAT_ID is required")
	}
	if c.AdminChatID <= 0 {
		return fmt.Errorf("ADMIN_CHAT_ID is required and must be a valid numeric chat ID")
	}
	if c.CronIntervalMinutes <= 0 {
		return fmt.Errorf("CRON_INTERVAL_MINUTES must be a positive integer")
	}
	if c.AlertIntervalSeconds <= 0 {
		return fmt.Errorf("ALERT_INTERVAL_SECONDS must be a positive integer")
	}
	return nil
}

// GetDVRSnapshotURL returns the full URL for capturing a snapshot from a specific camera
// Channel format: 101 = Cam 1, 201 = Cam 2, etc. (Main stream)
func (c *Config) GetDVRSnapshotURL(camNumber int) string {
	channel := camNumber*100 + 1 // 101, 201, 301, etc.
	return fmt.Sprintf("http://%s:%s/ISAPI/Streaming/channels/%d/picture",
		c.DVRIP, c.DVRPort, channel)
}

// GetCameraName returns the display name for a camera
func (c *Config) GetCameraName(camNumber int) string {
	if name, ok := c.CameraNames[camNumber]; ok {
		return name
	}
	return fmt.Sprintf("Camera %d", camNumber)
}

// ParseCameras parses camera input string and returns list of camera numbers
// Supports: "all", "1", "1,3,5", "1-4", "1,3-5,7"
func ParseCameras(input string, maxCams int) ([]int, error) {
	input = strings.TrimSpace(strings.ToLower(input))

	if input == "" || input == "all" {
		cams := make([]int, maxCams)
		for i := 0; i < maxCams; i++ {
			cams[i] = i + 1
		}
		return cams, nil
	}

	selected := make([]bool, maxCams+1)
	parts := strings.Split(input, ",")

	for _, part := range parts {
		part = strings.TrimSpace(part)

		// Check for range (e.g., "1-4")
		if strings.Contains(part, "-") {
			rangeParts := strings.Split(part, "-")
			if len(rangeParts) != 2 {
				return nil, fmt.Errorf("invalid range format: %s", part)
			}

			start, err := strconv.Atoi(strings.TrimSpace(rangeParts[0]))
			if err != nil {
				return nil, fmt.Errorf("invalid number: %s", rangeParts[0])
			}

			end, err := strconv.Atoi(strings.TrimSpace(rangeParts[1]))
			if err != nil {
				return nil, fmt.Errorf("invalid number: %s", rangeParts[1])
			}

			if start > end {
				start, end = end, start
			}

			for i := start; i <= end; i++ {
				if i >= 1 && i <= maxCams {
					selected[i] = true
				}
			}
		} else {
			// Single number
			num, err := strconv.Atoi(part)
			if err != nil {
				return nil, fmt.Errorf("invalid camera number: %s", part)
			}
			if num >= 1 && num <= maxCams {
				selected[num] = true
			}
		}
	}

	count := 0
	for i := 1; i <= maxCams; i++ {
		if selected[i] {
			count++
		}
	}
	if count == 0 {
		return nil, fmt.Errorf("no valid cameras specified")
	}

	// Convert set to sorted slice
	cams := make([]int, 0, count)
	for i := 1; i <= maxCams; i++ {
		if selected[i] {
			cams = append(cams, i)
		}
	}

	return cams, nil
}

// getEnv retrieves an environment variable or returns a default value
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
