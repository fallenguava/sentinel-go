# Sentinel-GO

A local background worker service that captures snapshots from a local CCTV DVR (Hikvision/HVR 4.0 OEM) and sends the image to a Telegram Chat via a Telegram Bot.

## Features

- 📷 Captures CCTV snapshots via ISAPI (Hikvision compatible)
- 🔐 Supports Digest Authentication for DVR access
- 📱 Sends images to Telegram with timestamps
- ⏰ Configurable capture interval
- 🚀 Startup and shutdown notifications
- ❌ Error notifications on capture failures
- 🔄 Graceful shutdown handling

## Project Structure

```
sentinel-go/
├── cmd/
│   └── sentinel/
│       └── main.go          # Entry point
├── internal/
│   ├── config/
│   │   └── config.go        # Configuration loader
│   ├── cctv/
│   │   └── cctv.go          # DVR/CCTV client with Digest Auth
│   └── telegram/
│       └── telegram.go      # Telegram Bot API client
├── .env.example             # Environment variables template
├── go.mod
├── go.sum
└── README.md
```

## Prerequisites

- Go 1.21 or later
- Access to a Hikvision-compatible DVR on your local network
- A Telegram Bot Token and Chat ID

## Installation

1. Clone the repository:

   ```bash
   git clone <repository-url>
   cd sentinel-go
   ```

2. Install dependencies:

   ```bash
   go mod download
   ```

3. Copy the example environment file and configure it:

   ```bash
   cp .env.example .env
   ```

4. Edit `.env` with your actual credentials:
   ```env
   DVR_IP=192.168.1.2
   DVR_PORT=80
   DVR_USER=admin
   DVR_PASS=your_dvr_password
   DVR_CHANNEL=101
   TELEGRAM_BOT_TOKEN=your_bot_token
   TELEGRAM_CHAT_ID=your_chat_id
   CRON_INTERVAL_MINUTES=60
   ```

## Configuration

| Variable                | Description                                  | Default       |
| ----------------------- | -------------------------------------------- | ------------- |
| `DVR_IP`                | IP address of your DVR                       | `192.168.1.2` |
| `DVR_PORT`              | HTTP port of your DVR                        | `80`          |
| `DVR_USER`              | DVR username                                 | (required)    |
| `DVR_PASS`              | DVR password                                 | (required)    |
| `DVR_CHANNEL`           | Camera channel number (e.g., 101 = Camera 1) | `101`         |
| `TELEGRAM_BOT_TOKEN`    | Your Telegram Bot API token                  | (required)    |
| `TELEGRAM_CHAT_ID`      | Target Telegram chat ID                      | (required)    |
| `CRON_INTERVAL_MINUTES` | Capture interval in minutes                  | `60`          |

### Camera Channel Mapping

For Hikvision DVRs, the channel format is typically:

- `101` = Camera 1, Main Stream
- `102` = Camera 1, Sub Stream
- `201` = Camera 2, Main Stream
- `202` = Camera 2, Sub Stream

## Running the Application

### Development Mode

```bash
go run ./cmd/sentinel/
```

### Build and Run

```bash
# Build the binary
go build -o bin/sentinel ./cmd/sentinel/

# Run the binary
./bin/sentinel
```

### Run as a Background Service (Linux/systemd)

1. Create a systemd service file:

   ```bash
   sudo nano /etc/systemd/system/sentinel-go.service
   ```

2. Add the following content:

   ```ini
   [Unit]
   Description=Sentinel-GO CCTV Snapshot Service
   After=network.target

   [Service]
   Type=simple
   User=your_username
   WorkingDirectory=/path/to/sentinel-go
   ExecStart=/path/to/sentinel-go/bin/sentinel
   Restart=always
   RestartSec=10

   [Install]
   WantedBy=multi-user.target
   ```

3. Enable and start the service:

   ```bash
   sudo systemctl daemon-reload
   sudo systemctl enable sentinel-go
   sudo systemctl start sentinel-go
   ```

4. Check the status:
   ```bash
   sudo systemctl status sentinel-go
   sudo journalctl -u sentinel-go -f
   ```

## Getting Your Telegram Bot Token and Chat ID

### Create a Telegram Bot

1. Open Telegram and search for `@BotFather`
2. Send `/newbot` and follow the prompts
3. Copy the bot token provided

### Get Your Chat ID

1. Start a chat with your new bot
2. Send any message to the bot
3. Visit: `https://api.telegram.org/bot<YOUR_BOT_TOKEN>/getUpdates`
4. Find the `chat.id` in the response JSON

## License

MIT License
