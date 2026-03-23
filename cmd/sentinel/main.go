package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"sentinel-go/internal/bot"
	"sentinel-go/internal/cctv"
	"sentinel-go/internal/config"
	"sentinel-go/internal/database"
	"sentinel-go/internal/telegram"
	"sentinel-go/internal/webcam"
)

func main() {
	log.Println("==============================================")
	log.Println("  SENTINEL-GO - CCTV Snapshot Service")
	log.Println("  With Authentication & Telegram Commands")
	log.Println("==============================================")

	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("[MAIN] Failed to load configuration: %v", err)
	}

	log.Printf("[MAIN] Configuration loaded:")
	log.Printf("[MAIN]   DVR: %s:%s (%d cameras)", cfg.DVRIP, cfg.DVRPort, cfg.NumCams)
	log.Printf("[MAIN]   Database: %s:%s/%s", cfg.DBHost, cfg.DBPort, cfg.DBName)
	log.Printf("[MAIN]   Default Interval: %d minutes", cfg.CronIntervalMinutes)

	// Initialize database
	db, err := database.New(cfg.DBHost, cfg.DBPort, cfg.DBUser, cfg.DBPass, cfg.DBName)
	if err != nil {
		log.Fatalf("[MAIN] FATAL: Database connection failed: %v", err)
	}
	defer db.Close()

	// Initialize clients
	cctvClient := cctv.NewClient(cfg.DVRIP, cfg.DVRPort, cfg.DVRUser, cfg.DVRPass)
	webcamClient := webcam.NewClient(cfg.WebcamDeviceName, cfg.WebcamFFmpegPath, cfg.WebcamCaptureDir)
	telegramClient := telegram.NewClient(cfg.TelegramBotToken, cfg.TelegramChatID)

	// Create a context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Perform health checks on startup
	log.Println("[MAIN] Running startup health checks...")

	if err := telegramClient.HealthCheck(ctx); err != nil {
		log.Fatalf("[MAIN] FATAL: Telegram health check failed: %v", err)
	}

	if err := cctvClient.HealthCheck(ctx); err != nil {
		log.Printf("[MAIN] WARNING: DVR health check failed: %v", err)
		log.Println("[MAIN] Will continue anyway - DVR might become available later")
	}

	if err := webcamClient.HealthCheck(ctx); err != nil {
		log.Printf("[MAIN] WARNING: Webcam health check failed: %v", err)
		log.Println("[MAIN] Will continue anyway - Webcam might become available later")
	}

	// Create and start the bot
	botInstance := bot.NewBot(cfg, db, cctvClient, webcamClient, telegramClient)
	botInstance.Start(ctx)

	log.Println("[MAIN] Bot is now running. Waiting for authenticated users...")

	// Setup signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Wait for shutdown signal
	sig := <-sigChan
	log.Printf("[MAIN] Received signal: %v", sig)
	log.Println("[MAIN] Initiating graceful shutdown...")

	// Stop the bot
	botInstance.Stop()
	cancel()

	log.Println("[MAIN] Sentinel-GO stopped. Goodbye!")
}
