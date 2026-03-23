package webcam

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"
)

// Client captures frames from the laptop webcam via PowerShell + ffmpeg (dshow) from WSL
type Client struct {
	deviceName string // e.g. "Integrated Webcam" — the dshow device name
	ffmpegPath string // Windows path to ffmpeg.exe, e.g. C:\tools\ffmpeg\bin\ffmpeg.exe
	captureDir string // Windows base path for captures, e.g. F:\SentinelGo\webcam_captures
}

// NewClient creates a new webcam client
func NewClient(deviceName, ffmpegPath, captureDir string) *Client {
	return &Client{
		deviceName: deviceName,
		ffmpegPath: ffmpegPath,
		captureDir: captureDir,
	}
}

// CaptureFrame opens the webcam, grabs 1 frame, closes it, returns JPEG bytes.
// File is stored in the configured capture directory with format:
//
//	sentinel_cam_20060102_150405_<6-char random hex>.jpg
//
// Windows path used for ffmpeg output, WSL path used to read the file.
// Files are kept permanently as capture history.
// Returns (imageBytes, "image/jpeg", error)
func (c *Client) CaptureFrame(ctx context.Context) ([]byte, string, error) {
	if c.deviceName == "" || c.ffmpegPath == "" || c.captureDir == "" {
		return nil, "", fmt.Errorf("webcam device, ffmpeg path, or capture dir not configured")
	}

	// Generate filename with timestamp and random suffix to prevent collisions
	timestamp := time.Now().Format("20060102_150405")
	randomSuffix := generateRandomHex(3)
	filename := fmt.Sprintf("sentinel_cam_%s_%s.jpg", timestamp, randomSuffix)

	// Build full Windows and WSL paths
	winFilePath := filepath.Join(c.captureDir, filename)
	wslFilePath := winPathToWSL(winFilePath)

	log.Printf("[WEBCAM] Capturing frame to %s", winFilePath)

	// Build ffmpeg command to run via PowerShell
	// Use backslashes with proper quoting since we'll use -EncodedCommand
	ffmpegCmd := fmt.Sprintf(`& '%s' -y -f dshow -i "video=%s" -vframes 1 -q:v 2 -update 1 "%s"`,
		c.ffmpegPath, c.deviceName, winFilePath)

	// Encode command as UTF-16 LE base64 for PowerShell's -EncodedCommand
	// This bypasses PowerShell's string parsing, avoiding backslash issues
	encoded, err := encodeForPowerShell(ffmpegCmd)
	if err != nil {
		return nil, "", fmt.Errorf("failed to encode command: %w", err)
	}

	// Create PowerShell command with encoded version
	cmd := exec.CommandContext(ctx, "/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe", "-NoProfile", "-EncodedCommand", encoded)

	// Run the command with output capture for logging
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, "", fmt.Errorf("ffmpeg capture failed: %w\nffmpeg output: %s", err, string(output))
	}

	log.Printf("[WEBCAM] Frame captured successfully, reading from %s", wslFilePath)

	// Read the file from WSL
	imageData, err := os.ReadFile(wslFilePath)
	if err != nil {
		return nil, "", fmt.Errorf("failed to read captured image: %w", err)
	}

	if len(imageData) == 0 {
		return nil, "", fmt.Errorf("captured image is empty")
	}

	log.Printf("[WEBCAM] Successfully read image (%d bytes), saved to %s", len(imageData), winFilePath)

	return imageData, "image/jpeg", nil
}

// HealthCheck tries to capture a frame to verify the webcam is accessible
func (c *Client) HealthCheck(ctx context.Context) error {
	if c.deviceName == "" || c.ffmpegPath == "" {
		log.Println("[WEBCAM] Webcam not configured, skipping health check")
		return nil
	}

	log.Println("[WEBCAM] Performing health check...")

	// Try a quick capture with a short timeout
	healthCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	_, _, err := c.CaptureFrame(healthCtx)
	if err != nil {
		return fmt.Errorf("webcam health check failed: %w", err)
	}

	log.Println("[WEBCAM] Health check passed")
	return nil
}

// winPathToWSL converts a Windows path to its WSL equivalent
// Example: C:\Windows\Temp\file.jpg -> /mnt/c/Windows/Temp/file.jpg
func winPathToWSL(p string) string {
	// Replace backslashes with forward slashes
	p = strings.ReplaceAll(p, "\\", "/")

	// Handle drive letters (e.g., C: -> /mnt/c)
	if len(p) >= 2 && p[1] == ':' {
		drive := strings.ToLower(string(p[0]))
		p = fmt.Sprintf("/mnt/%s%s", drive, p[2:])
	}

	return p
}

// encodeForPowerShell encodes a command string as UTF-16 LE base64 for PowerShell's -EncodedCommand
// This bypasses PowerShell's string parsing, avoiding backslash escape issues
func encodeForPowerShell(cmd string) (string, error) {
	// Convert to UTF-16 LE (PowerShell's native encoding)
	utf16Bytes := utf16.Encode([]rune(cmd))
	// Convert UTF-16 uint16 slice to byte slice (little-endian)
	byteArray := make([]byte, len(utf16Bytes)*2)
	for i, v := range utf16Bytes {
		byteArray[i*2] = byte(v)
		byteArray[i*2+1] = byte(v >> 8)
	}
	// Encode to base64
	encoded := base64.StdEncoding.EncodeToString(byteArray)
	return encoded, nil
}

// generateRandomHex generates n random bytes and returns them as a hex string
func generateRandomHex(numBytes int) string {
	buffer := make([]byte, numBytes)
	if _, err := rand.Read(buffer); err != nil {
		// Fallback to timestamp-based suffix if rand fails
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buffer)
}
