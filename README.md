# igdm-go

An Instagram DM bot system built in Go, using [mautrix-meta/messagix](https://go.mau.fi/mautrix-meta) — the same Go library that Beeper uses to bridge Instagram DMs.

## Features

- **Real-time DM monitoring** via Instagram's MQTT-over-WebSocket + Facebook LightSpeed binary sync protocol
- **LLM-powered auto-replies** — configure per-account personalities using any OpenAI-compatible API
- **Multi-account support** — run multiple bots simultaneously
- **Session persistence** — cookies and sync cursors survive restarts
- **Group chat support** — bots respond when mentioned or replied to
- **Following filter** — ignore DMs from non-followed accounts

## Architecture

Under the hood, `igdm-go` uses `go.mau.fi/mautrix-meta/pkg/messagix` which implements Instagram's web messaging protocol. Key design patterns borrowed from Beeper:

- **PostHandlePublishResponse** called after every publish event to advance sync group cursors (without this, Instagram stops pushing new messages)
- **State persistence** via `DumpState`/`LoadState` preserves sync cursors across restarts
- **Composite event handler** set once in `Connect`, never replaced

## Setup

### 1. Clone and build

```bash
git clone https://github.com/ocythoe/igdm-go.git
cd igdm-go
go build -o igdm .
```

### 2. Configure personality

```bash
mkdir -p ~/.igdm
cp personality.json.example ~/.igdm/personality.json
```

Edit `~/.igdm/personality.json` with your account names, personalities, and contact info:

```json
{
  "personalities": {
    "your_account": "You are a friendly chatbot on Instagram...",
  },
  "known_contacts": {
    "123456789": "username_example"
  },
  "bot_sender_ids": {
    "your_account": 12345678901234567
  }
}
```

### 3. Configure accounts

Create `~/.igdm/config.json`:

```json
{
  "accounts": {
    "your_account": {
      "username": "your_username",
      "password": "your_password"
    }
  },
  "llm": {
    "base_url": "https://open.bigmodel.cn/api/coding/paas/v4",
    "api_key_env": "GLM_API_KEY",
    "model": "glm-5.1"
  }
}
```

### 4. Set API key

```bash
export GLM_API_KEY="your-api-key"
```

### 5. Login

```bash
./igdm login your_account
```

### 6. Run

```bash
# Auto-reply on a single account
./igdm agent your_account

# Auto-reply on all accounts
./igdm agent-all

# Just listen (no auto-reply)
./igdm listen your_account
```

## Commands

```
igdm login <account>                      Login and save session
igdm send <account> <user> <message>      Send a DM
igdm listen <account>                     Listen for incoming messages
igdm listen-all                           Listen on all accounts
igdm agent <account>                      Auto-reply to DMs using LLM
igdm agent-all                            Auto-reply on all accounts
igdm whoami <account>                     Show logged in user info
igdm threads <account>                    List recent threads
igdm debug <account> [other_account]      Debug/diagnostic output
```

## Configuration Reference

### personality.json

| Field | Type | Description |
|-------|------|-------------|
| `personalities` | `map[string]string` | Account name → system prompt for LLM |
| `known_contacts` | `map[string]string` | Instagram sender ID → display name |
| `bot_sender_ids` | `map[string]int64` | Account name → MQTT sender ID (for bot loop prevention) |

### config.json

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `data_dir` | `string` | `~/.igdm` | Directory for sessions and data |
| `accounts` | `map` | — | Account credentials |
| `llm.base_url` | `string` | BigModel API | OpenAI-compatible API endpoint |
| `llm.api_key_env` | `string` | `GLM_API_KEY` | Environment variable for API key |
| `llm.model` | `string` | `glm-5.1` | Model name |
| `llm.max_history` | `int` | `50` | Max conversation history per thread |
| `llm.response_delay_min_sec` | `int` | `2` | Min typing delay before reply |
| `llm.response_delay_max_sec` | `int` | `8` | Max typing delay before reply |

## License

MIT
