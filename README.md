# igdm-go

An Instagram DM bot system built in Go, using [mautrix-meta/messagix](https://go.mau.fi/mautrix-meta) — the same Go library that Beeper uses to bridge Instagram DMs.

## Features

- **Real-time DM monitoring** via Instagram's MQTT-over-WebSocket + Facebook LightSpeed binary sync protocol
- **Send & receive DMs** from the command line
- **LLM-powered auto-replies** — configure per-account personalities using any OpenAI-compatible API
- **Multi-account support** — run multiple bots simultaneously
- **Session persistence** — cookies and sync cursors survive restarts
- **Group chat support** — bots respond when mentioned or replied to
- **Following filter** — ignore DMs from non-followed accounts
- **Thread listing & history** — browse conversations from the CLI
- **Config management** — view and modify settings without editing JSON

## Quick Start

### 1. Clone and build

```bash
git clone https://github.com/ocythoe/igdm-go.git
cd igdm-go
go build -o igdm .
```

### 2. Configure accounts

Create `~/.igdm/config.json`:

```json
{
  "accounts": {
    "myaccount": {
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

### 3. Set API key (for agent mode)

```bash
export GLM_API_KEY="your-api-key"
```

### 4. Login

```bash
./igdm login myaccount
```

### 5. Use

```bash
# Send a message
./igdm send myaccount friend_username "Hello there!"

# List recent threads
./igdm threads myaccount

# View message history
./igdm history myaccount 123456789 20

# Listen for incoming messages
./igdm listen myaccount

# Start auto-reply agent
./igdm agent myaccount

# Debug with verbose logging
./igdm --verbose threads myaccount
```

## Commands

### Global Flags

| Flag | Short | Description |
|------|-------|-------------|
| `--verbose` | `-v` | Enable debug-level logging for all commands |

### `login <account>`

Authenticate with Instagram and save the session. The session (cookies + sync state) is stored at `~/.igdm/<account>.json`.

```bash
./igdm login myaccount
```

### `send <account> <username> <message>`

Send a DM to an Instagram user by their username.

```bash
./igdm send myaccount friend_username "Hey, what's up?"
```

### `listen <account>`

Connect to Instagram's real-time MQTT stream and print incoming messages to stdout. Press Ctrl+C to stop.

```bash
./igdm listen myaccount
```

### `listen-all`

Listen for messages on all configured accounts simultaneously.

```bash
./igdm listen-all
```

### `whoami <account>`

Display information about the logged-in account.

```bash
./igdm whoami myaccount
# Output:
#   Account: myaccount
#     Username: your_username
#     Name: Your Name
#     User ID: 123456789
#     Avatar: https://...
```

### `threads <account>`

List recent DM threads from your inbox.

```bash
./igdm threads myaccount
# Output:
#   Threads for @myaccount:
#     1. friend_username (key=123456789)
#     2. Group Chat Name (key=-987654321) [GROUP]
```

### `history <account> <thread_key> [limit]`

Show message history for a specific thread. Use the `thread_key` from the `threads` command. Default limit is 20 messages.

```bash
# Show last 20 messages
./igdm history myaccount 123456789

# Show last 50 messages
./igdm history myaccount 123456789 50
```

### `config show`

Display the current configuration with passwords masked.

```bash
./igdm config show
```

### `config set <key> <value>`

Modify a configuration value. The config file is updated immediately.

```bash
./igdm config set llm.model glm-5.1
./igdm config set llm.max_history 100
./igdm config set llm.response_delay_min_sec 3
```

Supported keys:
- `llm.model` — LLM model name
- `llm.base_url` — LLM API base URL
- `llm.api_key_env` — Environment variable name for API key
- `llm.max_history` — Max conversation history per thread (integer)
- `llm.response_delay_min_sec` — Min delay before reply in seconds (integer)
- `llm.response_delay_max_sec` — Max delay before reply in seconds (integer)

### `config accounts`

List all configured accounts with their login session status.

```bash
./igdm config accounts
# Output:
#   Configured accounts:
#     myaccount            @your_username         [session saved]
#     otheraccount         @other_username        [no session]
```

### `agent <account>`

Start an LLM-powered auto-reply bot that responds to incoming DMs. The bot:
- Responds to 1:1 DMs from followed accounts
- Responds in group chats only when @mentioned or replied to
- Skips its own messages and messages from known bot accounts
- Maintains per-thread conversation history
- Adds a random human-like delay before replying
- Sends typing indicators while "thinking"

```bash
./igdm agent myaccount
```

### `agent-all`

Run the auto-reply agent on all configured accounts simultaneously.

### `debug <account> [other_account]`

Diagnostic command that dumps initial sync data, thread info, contacts, and optionally sends a test message. Useful for troubleshooting connection issues.

```bash
./igdm debug myaccount
./igdm debug myaccount other_account
```

## Personality Configuration

Create `~/.igdm/personality.json` to customize bot behavior per account:

```json
{
  "personalities": {
    "myaccount": "You are a friendly and casual Instagram user. Keep responses short and natural, like texting a friend.",
    "workaccount": "You are a professional assistant. Be polite, concise, and helpful."
  },
  "known_contacts": {
    "123456789": "john_doe",
    "987654321": "jane_smith"
  },
  "bot_sender_ids": {
    "myaccount": 0,
    "workaccount": 0
  }
}
```

See `personality.json.example` for a template.

| Field | Type | Description |
|-------|------|-------------|
| `personalities` | `map[string]string` | Account name → system prompt for LLM |
| `known_contacts` | `map[string]string` | Instagram sender ID → display name |
| `bot_sender_ids` | `map[string]int64` | Account name → MQTT sender ID (for bot loop prevention) |

## Configuration Reference

### config.json

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `data_dir` | `string` | `~/.igdm` | Directory for sessions, memory, and personality |
| `accounts` | `map` | — | Account credentials (name → {username, password}) |
| `llm.base_url` | `string` | `https://open.bigmodel.cn/api/coding/paas/v4` | OpenAI-compatible API endpoint |
| `llm.api_key_env` | `string` | `GLM_API_KEY` | Environment variable to read API key from |
| `llm.model` | `string` | `glm-5.1` | Model name for chat completions |
| `llm.max_history` | `int` | `50` | Max conversation history messages per thread |
| `llm.response_delay_min_sec` | `int` | `2` | Minimum random delay before reply (seconds) |
| `llm.response_delay_max_sec` | `int` | `8` | Maximum random delay before reply (seconds) |

## Architecture

```
main.go                    CLI entry point, command dispatch
├── internal/
│   ├── client/
│   │   ├── client.go      IGClient wrapper around messagix
│   │   └── following.go   Following cache from MQTT sync data
│   ├── config/
│   │   └── config.go      Configuration & session persistence
│   ├── listener/
│   │   └── listener.go    Event dispatcher (MQTT → handlers)
│   ├── login/
│   │   └── login.go       Instagram web auth (username+password → cookies)
│   └── agent/
│       ├── agent.go       LLM agent (OpenAI-compatible chat API)
│       └── memory.go      Per-thread conversation memory (JSON on disk)
```

### How it works

Under the hood, `igdm-go` uses `go.mau.fi/mautrix-meta/pkg/messagix` which implements Instagram's web messaging protocol:

1. **Login**: Authenticates via Instagram's web API to obtain session cookies
2. **LoadMessagesPage**: Fetches the initial HTML page to extract sync data (threads, contacts, messages)
3. **MQTT Connect**: Establishes a WebSocket connection to Instagram's MQTT broker
4. **LightSpeed Sync**: Receives real-time updates via Facebook's binary sync protocol
5. **Event Processing**: A composite handler dispatches events (messages, typing, thread updates) to registered handlers

### Key design patterns (from Beeper)

- **PostHandlePublishResponse**: Called after every publish event to advance sync group cursors. Without this, Instagram's server stops pushing new messages because the client never acknowledges its cursor position.
- **State persistence**: `DumpState`/`LoadState` preserves sync cursors across restarts so the server knows where to resume pushing deltas.
- **Composite event handler**: Set once in `Connect()` and never replaced. User handlers are forwarded through this single handler, ensuring cursor updates and ready tracking are never lost.

## Known Limitations

### Real-time MQTT receiving

The MQTT connection establishes and receives the initial `Event_Ready` sync, but **incoming real-time messages may not arrive** in some sessions. This is a known issue that requires runtime testing with real accounts to diagnose. Possible causes:

1. **Missing foreground state report** — Instagram requires apps to report they're in the foreground via `ReportAppState(FOREGROUND)`. The messagix library handles this during a fresh connect but may not on reconnects.
2. **State loading vs. fresh connect** — Loading saved state sets `previouslyConnected=true` which takes the MQTT "reconnect" path, potentially skipping `/ls_resp` subscription and the foreground report.
3. **Sync group cursor desync** — If cursors aren't properly advanced, Instagram stops pushing deltas.

**Current workaround**: The code forces a fresh connect (state loading is disabled). This means the full MQTT handshake runs each time, including topic subscriptions and foreground reporting. State is still saved on disconnect for potential future use.

### Other limitations

- **Two-factor authentication** is not supported
- **Media messages** (images, videos, voice) are not handled
- **Tab completion** is not yet implemented
- **Rate limiting** handling is minimal — aggressive use may trigger Instagram's anti-abuse systems
- Sessions expire periodically and require re-login

## Troubleshooting

### "load config" error on startup

Make sure `~/.igdm/config.json` exists with at least one account configured.

### Login fails with "missing cookies"

Instagram may have changed its login flow. Try:
1. Clearing `~/.igdm/<account>.json` (the saved session)
2. Running `./igdm login <account>` again
3. If the account has 2FA enabled, it's not supported yet

### "not connected" errors

The session cookies may have expired. Delete the session file and re-login:
```bash
rm ~/.igdm/<account>.json
./igdm login <account>
```

### No incoming messages in listen mode

This is the known MQTT receiving issue (see above). Try:
1. Use `--verbose` flag to see all MQTT events: `./igdm --verbose listen <account>`
2. Run `./igdm debug <account>` to check initial sync data
3. Delete the session file and re-login (forces fresh MQTT handshake)

### "LLM API key not set"

Set the environment variable:
```bash
export GLM_API_KEY="your-api-key"
```
Or configure a different env var: `./igdm config set llm.api_key_env MY_API_KEY_VAR`

### Debug logging

Add `--verbose` or `-v` to any command for detailed debug output:
```bash
./igdm -v listen myaccount
./igdm --verbose threads myaccount
```

## Data Storage

All data is stored in `~/.igdm/` (configurable via `data_dir`):

```
~/.igdm/
├── config.json              Account credentials and LLM settings
├── personality.json         Per-account prompts and contact mappings
├── <account>.json           Session data (cookies + sync state)
└── memory/
    └── <thread_id>/
        ├── 2025-01-15.json  Daily conversation history
        ├── 2025-01-16.json
        └── summary.md       Running conversation summary
```

## License

MIT
