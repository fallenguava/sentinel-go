package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Client handles communication with the Telegram Bot API
type Client struct {
	httpClient *http.Client
	botToken   string
	chatID     string
	apiBaseURL string
}

// APIResponse represents a Telegram API response
type APIResponse struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
}

// Update represents a Telegram update (incoming message)
type Update struct {
	UpdateID int      `json:"update_id"`
	Message  *Message `json:"message,omitempty"`
}

// Message represents a Telegram message
type Message struct {
	MessageID int    `json:"message_id"`
	Chat      Chat   `json:"chat"`
	Text      string `json:"text"`
	From      *User  `json:"from,omitempty"`
}

// Chat represents a Telegram chat
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

// User represents a Telegram user
type User struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	Username  string `json:"username,omitempty"`
}

// Command represents a parsed bot command
type Command struct {
	Name string
	Args string
}

// NewClient creates a new Telegram client
func NewClient(botToken, chatID string) *Client {
	return &Client{
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
		botToken:   botToken,
		chatID:     chatID,
		apiBaseURL: "https://api.telegram.org",
	}
}

// GetChatID returns the configured chat ID
func (c *Client) GetChatID() string {
	return c.chatID
}

// SendPhoto sends an image to the configured Telegram chat
func (c *Client) SendPhoto(ctx context.Context, imageData []byte, contentType, caption string) error {
	return c.SendPhotoToChat(ctx, c.chatID, imageData, contentType, caption)
}

// SendPhotoToChat sends an image to a specific chat
func (c *Client) SendPhotoToChat(ctx context.Context, chatID string, imageData []byte, contentType, caption string) error {
	log.Printf("[TELEGRAM] Preparing to send photo (%d bytes) to chat %s", len(imageData), chatID)

	apiURL := fmt.Sprintf("%s/bot%s/sendPhoto", c.apiBaseURL, c.botToken)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	if err := writer.WriteField("chat_id", chatID); err != nil {
		return fmt.Errorf("failed to write chat_id field: %w", err)
	}

	if caption != "" {
		if err := writer.WriteField("caption", caption); err != nil {
			return fmt.Errorf("failed to write caption field: %w", err)
		}
		// Enable HTML parsing for caption
		if err := writer.WriteField("parse_mode", "HTML"); err != nil {
			return fmt.Errorf("failed to write parse_mode field: %w", err)
		}
	}

	filename := getFilenameFromContentType(contentType)
	part, err := writer.CreateFormFile("photo", filename)
	if err != nil {
		return fmt.Errorf("failed to create form file: %w", err)
	}

	if _, err := part.Write(imageData); err != nil {
		return fmt.Errorf("failed to write image data: %w", err)
	}

	if err := writer.Close(); err != nil {
		return fmt.Errorf("failed to close multipart writer: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, body)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}

	var apiResp APIResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return fmt.Errorf("failed to parse API response: %w", err)
	}

	if !apiResp.OK {
		return fmt.Errorf("Telegram API error: %s", apiResp.Description)
	}

	log.Printf("[TELEGRAM] Successfully sent photo to chat %s", chatID)
	return nil
}

// SendMessage sends a text message to the configured Telegram chat
func (c *Client) SendMessage(ctx context.Context, message string) error {
	return c.SendMessageToChat(ctx, c.chatID, message)
}

// SendMessageToChat sends a text message to a specific chat
func (c *Client) SendMessageToChat(ctx context.Context, chatID string, message string) error {
	log.Printf("[TELEGRAM] Sending message to chat %s", chatID)

	apiURL := fmt.Sprintf("%s/bot%s/sendMessage", c.apiBaseURL, c.botToken)

	payload := map[string]interface{}{
		"chat_id":    chatID,
		"text":       message,
		"parse_mode": "HTML",
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}

	var apiResp APIResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return fmt.Errorf("failed to parse API response: %w", err)
	}

	if !apiResp.OK {
		return fmt.Errorf("Telegram API error: %s", apiResp.Description)
	}

	log.Printf("[TELEGRAM] Successfully sent message to chat %s", chatID)
	return nil
}

// GetUpdates fetches new messages from Telegram using long polling
func (c *Client) GetUpdates(ctx context.Context, offset int, timeout int) ([]Update, error) {
	apiURL := fmt.Sprintf("%s/bot%s/getUpdates?offset=%d&timeout=%d",
		c.apiBaseURL, c.botToken, offset, timeout)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Use a longer client timeout for long polling
	client := &http.Client{
		Timeout: time.Duration(timeout+10) * time.Second,
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get updates: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	var apiResp struct {
		OK     bool     `json:"ok"`
		Result []Update `json:"result"`
	}

	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return nil, fmt.Errorf("failed to parse API response: %w", err)
	}

	if !apiResp.OK {
		return nil, fmt.Errorf("Telegram API error getting updates")
	}

	return apiResp.Result, nil
}

// ParseCommand extracts command and arguments from a message text
func ParseCommand(text string) *Command {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return nil
	}

	// Remove the leading slash
	text = text[1:]

	// Split into command and args
	parts := strings.SplitN(text, " ", 2)
	cmd := &Command{
		Name: strings.ToLower(strings.Split(parts[0], "@")[0]), // Remove @botname suffix
	}

	if len(parts) > 1 {
		cmd.Args = strings.TrimSpace(parts[1])
	}

	return cmd
}

// IsAuthorizedChat checks if the chat ID is authorized to use the bot
func (c *Client) IsAuthorizedChat(chatID int64) bool {
	authorizedID, err := strconv.ParseInt(c.chatID, 10, 64)
	if err != nil {
		return false
	}
	return chatID == authorizedID
}

// HealthCheck verifies the bot token is valid
func (c *Client) HealthCheck(ctx context.Context) error {
	log.Println("[TELEGRAM] Performing health check...")

	apiURL := fmt.Sprintf("%s/bot%s/getMe", c.apiBaseURL, c.botToken)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return fmt.Errorf("health check failed - cannot create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("health check failed - cannot reach Telegram API: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("health check failed - cannot read response: %w", err)
	}

	var apiResp APIResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return fmt.Errorf("health check failed - cannot parse response: %w", err)
	}

	if !apiResp.OK {
		return fmt.Errorf("health check failed - invalid bot token: %s", apiResp.Description)
	}

	log.Println("[TELEGRAM] Health check passed - Bot token is valid")
	return nil
}

func getFilenameFromContentType(contentType string) string {
	switch contentType {
	case "image/png":
		return "snapshot.png"
	case "image/gif":
		return "snapshot.gif"
	default:
		return "snapshot.jpg"
	}
}
