package cctv

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/icholy/digest"
)

// Client handles communication with the DVR/CCTV system
type Client struct {
	httpClient *http.Client
	baseURL    string
	dvrIP      string
	dvrPort    string
	username   string
	password   string
}

// NewClient creates a new CCTV client with digest authentication
func NewClient(dvrIP, dvrPort, username, password string) *Client {
	// Create a transport with digest authentication
	transport := &digest.Transport{
		Username: username,
		Password: password,
	}

	return &Client{
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
		},
		dvrIP:    dvrIP,
		dvrPort:  dvrPort,
		username: username,
		password: password,
	}
}

// GetSnapshotURL returns the ISAPI URL for a specific camera
// Channel format: 101 = Cam 1 Main, 201 = Cam 2 Main, etc.
func (c *Client) GetSnapshotURL(camNumber int) string {
	channel := camNumber*100 + 1
	return fmt.Sprintf("http://%s:%s/ISAPI/Streaming/channels/%d/picture",
		c.dvrIP, c.dvrPort, channel)
}

// CaptureSnapshot fetches a snapshot image from a specific camera
// Returns the image bytes and content type, or an error if the capture fails
func (c *Client) CaptureSnapshot(ctx context.Context, camNumber int) ([]byte, string, error) {
	url := c.GetSnapshotURL(camNumber)
	log.Printf("[CCTV] Starting snapshot capture from Camera %d: %s", camNumber, url)

	// Create request with context for timeout/cancellation support
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create request: %w", err)
	}

	// Execute the request
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("failed to fetch snapshot: %w", err)
	}
	defer resp.Body.Close()

	// Check for successful response
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, "", fmt.Errorf("DVR returned status %d: %s", resp.StatusCode, string(body))
	}

	// Read the image data
	imageData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("failed to read image data: %w", err)
	}

	// Validate we received data
	if len(imageData) == 0 {
		return nil, "", fmt.Errorf("received empty image data from DVR")
	}

	// Get content type (default to JPEG if not specified)
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "image/jpeg"
	}

	log.Printf("[CCTV] Successfully captured Camera %d (%d bytes, %s)", camNumber, len(imageData), contentType)
	return imageData, contentType, nil
}

// HealthCheck verifies connectivity to the DVR using Camera 1
func (c *Client) HealthCheck(ctx context.Context) error {
	log.Println("[CCTV] Performing health check...")

	url := c.GetSnapshotURL(1) // Use Camera 1 for health check
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("health check failed - cannot create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("health check failed - cannot reach DVR: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("health check failed - authentication error (check DVR_USER/DVR_PASS)")
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check failed - DVR returned status %d", resp.StatusCode)
	}

	log.Println("[CCTV] Health check passed - DVR is reachable")
	return nil
}
