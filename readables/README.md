# Sentinel-GO

A Go-based Telegram bot for monitoring Hikvision CCTV cameras with authentication, scheduled captures, and image collage generation.

## Overview

Sentinel-GO is a background worker that:

1. Connects to a Hikvision DVR at `192.168.1.2:80` via ISAPI (Digest Auth)
2. Captures snapshots from up to 7 cameras
3. Combines images into a single high-quality collage
4. Sends to Telegram with a bot interface
5. Supports scheduled auto-captures to all authorized users

## Architecture

```
cmd/sentinel/main.go          # Entry point, initializes all components

internal/
├── auth/auth.go              # Allowed names whitelist, validation, auth messages
├── bot/bot.go                # Telegram command handlers, scheduler, auth flow
├── cctv/cctv.go              # Hikvision ISAPI client with Digest Auth
├── config/config.go          # Environment config loader, camera name mapping
├── database/database.go      # PostgreSQL: authorized_users, auth_states tables
├── imaging/collage.go        # Image collage creation with bilinear scaling
└── telegram/telegram.go      # Telegram Bot API client (sendMessage, sendPhoto)

assets/
└── win.jpg                   # Photo displayed after successful auth

.env                          # Configuration (see Environment Variables)
```

## Authentication System

### Flow for New Users

1. User sends `/start`
2. Bot asks: "Who are you? Please tell me your name."
3. User provides name → validated against `AllowedNames` map in `auth.go`
4. If valid, bot asks: "What is Win's birthday month?"
5. User answers → validated against `ValidBirthdayMonths` (july, juli, 7, 07)
6. If correct → user added to `authorized_users` table with 7-day expiry
7. Bot sends success message + Win's photo with random praising message

### Flow for Expired Users

1. User sends any message
2. Bot detects expired session via `expires_at` column
3. Bot asks security question only (skips name step)
4. If correct → `expires_at` extended by 7 days via `RenewAuthorization()`

### Allowed Names (case-insensitive)

```
win, winanda, dian, arissa, hondi, memey, sri, sri mulyati, osang,
hendra, hendra gunawan, oksiang, hoksiang, santy, santi, herman,
herman subrata, osin, oksin, hoksin, ergi, egi, verlita, dqrren, darren, deren
```

### Security Features

- 3 failed attempts = 10 minute lockout
- 7-day session expiry (stored in `expires_at` column)
- Auth state machine stored in `auth_states` table (step, attempts, name)

## Database Schema (PostgreSQL)

```sql
-- Authorized users with expiry
CREATE TABLE authorized_users (
    chat_id BIGINT PRIMARY KEY,
    name TEXT NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    last_seen TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    expires_at TIMESTAMP NOT NULL  -- 7 days from authorization
);

-- Auth state machine for multi-step authentication
CREATE TABLE auth_states (
    chat_id BIGINT PRIMARY KEY,
    step INT DEFAULT 0,           -- 0=not started, 1=waiting name, 2=waiting answer, 10=renewal
    name TEXT DEFAULT '',
    attempts INT DEFAULT 0,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
```

## Telegram Commands

| Command          | Aliases             | Description                                               |
| ---------------- | ------------------- | --------------------------------------------------------- |
| `/start`         | -                   | Begin authentication (or welcome if already auth'd)       |
| `/help`          | `/h`                | Show all available commands                               |
| `/capture`       | `/c`, `/snap`       | Capture from enabled cameras                              |
| `/capture 1,3,5` | `/c 1-3`            | Capture specific cameras (supports ranges)                |
| `/list`          | `/cams`, `/cameras` | Show all cameras with enabled status                      |
| `/enable`        | `/on`               | Enable cameras (e.g., `/enable 1-7` or `/enable all`)     |
| `/disable`       | `/off`              | Disable cameras                                           |
| `/interval`      | `/int`              | Get/set capture interval in minutes                       |
| `/scheduler`     | `/sched`            | Toggle scheduler on/off                                   |
| `/status`        | -                   | Show scheduler status, interval, enabled cameras          |
| `/whoami`        | -                   | Show profile: name, chat ID, auth date, expiry, last seen |
| `/logout`        | -                   | Revoke access and delete from authorized_users            |
| `/ping`          | -                   | Health check                                              |

## CCTV Integration

### Hikvision ISAPI

- Endpoint: `http://{DVR_HOST}:{DVR_PORT}/ISAPI/Streaming/channels/{channel}01/picture`
- Authentication: HTTP Digest Auth
- Channel mapping: Camera 1 = channel 101, Camera 2 = channel 201, etc.

### Capture Flow

1. `cctv.CaptureSnapshot(ctx, camNumber)` makes HTTP GET with Digest Auth
2. Returns raw JPEG bytes and content type
3. Multiple cameras captured in parallel (for manual `/c`)
4. Single capture, send to all (for scheduler)

## Image Collage

### Configuration (in `imaging/collage.go`)

```go
HighQualityCollageConfig = CollageConfig{
    CellWidth:   800,   // Each camera image scaled to 800px wide
    CellHeight:  600,   // Each camera image scaled to 600px tall
    Padding:     10,    // Padding between images
    JPEGQuality: 95,    // High quality JPEG output
}
```

### Grid Layout

- 1 camera: 1x1
- 2 cameras: 2x1
- 3-4 cameras: 2x2
- 5-6 cameras: 3x2
- 7-9 cameras: 3x3

### Process

1. Decode each JPEG to `image.Image`
2. Scale using bilinear interpolation (custom implementation)
3. Draw onto single canvas with padding
4. Encode as JPEG with 95% quality
5. Add caption with camera names and timestamp

## Scheduler

### Behavior

- Runs in background goroutine
- When active: waits `intervalMinutes`, then captures from enabled cameras
- Sends collage to **all authorized users** (not just one)
- Controlled via `/scheduler on|off` command

### State (in `bot.go`)

```go
schedulerActive bool           // on/off
intervalMinutes int            // default from config, changeable via /interval
enabledCameras  map[int]bool   // which cameras to include
```

## Environment Variables (.env)

```env
# DVR Configuration
DVR_HOST=192.168.1.2
DVR_PORT=80
DVR_USERNAME=admin
DVR_PASSWORD=your_password
NUM_CAMS=7

# Camera Names (optional)
CAM_1_NAME=Teras Depan
CAM_2_NAME=Pintu Pagar
CAM_3_NAME=Carport
CAM_4_NAME=Teras Samping
CAM_5_NAME=Dapur
CAM_6_NAME=Ruang Tamu
CAM_7_NAME=Teras Belakang

# Telegram Configuration
TELEGRAM_BOT_TOKEN=7834792492:AAEM2_Lx6kwBSgW5uImZH3E_NLm5-uL5rO4
TELEGRAM_CHAT_ID=909854073

# Scheduler Defaults
DEFAULT_INTERVAL_MINUTES=60

# Database
DB_HOST=localhost
DB_PORT=5432
DB_USER=postgres
DB_PASSWORD=postgres
DB_NAME=sentinel
```

## Key Functions Reference

### bot/bot.go

- `Start(ctx)` - Main loop: starts scheduler goroutine + polling loop
- `handleUpdate(update)` - Routes to auth flow or command handler
- `handleAuth(chatID, text)` - State machine for new user auth
- `handleRenewal(chatID, text)` - Re-auth for expired users
- `handleCapture(chatID, args)` - Manual capture command
- `captureAndSend(chatID, cameras)` - Capture + collage + send
- `runScheduler(ctx)` - Background scheduler loop
- `sendScheduledCapture(cameras)` - Capture once, send to all users
- `sendPraisingImage(chatID, caption)` - Send win.jpg with message

### database/database.go

- `IsAuthorized(chatID)` - Check if user exists and not expired
- `IsExpired(chatID)` - Check if user exists but expired
- `AddAuthorizedUser(chatID, name)` - Insert with 7-day expiry
- `RenewAuthorization(chatID)` - Extend expiry by 7 days
- `GetAuthState(chatID)` - Get current auth flow state
- `UpdateAuthState(state)` - Save auth flow progress

### auth/auth.go

- `IsValidName(name)` - Check against AllowedNames map
- `IsValidBirthdayMonth(answer)` - Check against ValidBirthdayMonths
- `GetSuccessMessage(name)` - Returns welcome + random praise
- `GetExpiredMessage(name)` - Returns re-auth prompt
- `getRandomPraise()` - Random fun message about WIN

## Praising Messages (after auth)

After successful authentication or renewal, bot sends `assets/win.jpg` with a random message:

- "🔥 Shout out to WIN! He coded this while waiting for Babi Cin. Anjay!"
- "💻 Crafted by WIN during a late-night session fueled by Indomie..."
- "🏆 Shout out to WIN - turned caffeine and chaos into working software!"
- ... and 12 more variations

## Running the Application

```bash
# Build
go build -o bin/sentinel ./cmd/sentinel/

# Run (requires PostgreSQL running, .env configured)
./bin/sentinel
```

## Dependencies

```
github.com/joho/godotenv  # .env file loading
github.com/icholy/digest  # HTTP Digest Authentication
github.com/lib/pq         # PostgreSQL driver
```

---

_Built by WIN while waiting for Babi Cin_ 🔥
