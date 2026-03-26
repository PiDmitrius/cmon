# cmon

Real-time session monitor for [OpenClaw](https://github.com/openclaw/openclaw) agents. Web UI with end-to-end encryption -- works over plain HTTP, no TLS required.

**Install via AI agent:**
```
1) Read https://raw.githubusercontent.com/PiDmitrius/cmon/main/README.md
2) Install cmon and give me the URL and token.
```

## What it does

Parses OpenClaw session JSONL files, displays a live feed of User / Agent / Tool interactions. All traffic between server and browser is encrypted with [Cryptash](https://github.com/deemru/Cryptash) (SHA-256 CBC keystream).

## Quick start

```bash
go install github.com/PiDmitrius/cmon@latest
```

Create config with a token:

```bash
mkdir -p ~/.config/cmon
cat > ~/.config/cmon/config.json << 'EOF'
{
  "token": "your-secret-token"
}
EOF
```

Run:

```bash
cmon
```

```
http://0.0.0.0:18787
your-secret-token
```

Open the URL, paste the token, done. Ctrl+C to stop.

## Commands

| Command | Description |
|---------|-------------|
| `cmon` | Start web server (foreground, Ctrl+C to stop) |
| `cmon run` | Install and start as systemd user service |
| `cmon stop` | Stop and remove user service |
| `cmon cli` | CLI mode (foreground, stdout) |
| `cmon help` | Show help |

## Config

`~/.config/cmon/config.json` (or `$XDG_CONFIG_HOME/cmon/config.json`):

```json
{
  "token": "your-secret-token",
  "port": 18787,
  "dir": "/path/to/sessions"
}
```

- `token` -- required, encryption key for web UI
- `port` -- default 18787
- `dir` -- default `~/.openclaw/agents/main/sessions`

## CLI mode

`cmon cli` prints the session log directly to stdout with ANSI colors. No web server, no encryption -- just terminal output. Good for SSH sessions or piping into other tools.

```bash
cmon cli              # history + live tail
cmon cli -nowatch     # dump history and exit
cmon cli -notools     # hide tool calls/results
cmon cli -nocolor     # no ANSI colors (for piping)
```

Tool output is collapsed after 15 lines, matching the web UI spoiler behavior.

## For AI agents

When a user asks to set up cmon:

```bash
# Install
go install github.com/PiDmitrius/cmon@latest

# Create config
mkdir -p ~/.config/cmon
TOKEN=$(cat /dev/urandom | tr -dc 'a-zA-Z0-9' | head -c32)
cat > ~/.config/cmon/config.json << EOF
{
  "token": "$TOKEN"
}
EOF

# Start as service
cmon run
```

Then tell the user:
- URL: `http://<host>:18787`
- Token: the value from config

To update:

```bash
cmon stop
go install github.com/PiDmitrius/cmon@latest
cmon run
```

If `cmon` is not in PATH, use the full path: `~/go/bin/cmon run`.

The systemd service records the absolute path to the binary automatically.

## Architecture

```
Browser                           Server
  |                                  |
  |--> encrypt(cmd) --> POST /api -->|
  |                                  |--> decrypt(cmd)
  |                                  |    process(cmd)
  |                                  |<-- encrypt(response)
  |<-- decrypt(response) <-- HTTP <--|
```

- One endpoint: `POST /api` -- all traffic binary-encrypted
- Auth: encrypted nonce handshake proves shared key
- History: single encrypted JSON array, one decrypt on client
- Live updates: long polling (30s timeout), generation tracking for structural changes
- Cryptash: SHA-256 CBC stream cipher, 4-byte IV, 4-byte MAC
- SHA-256: Go stdlib on server, pure JS in browser (no `crypto.subtle` -- works in HTTP contexts)
- Sessions: parsed from OpenClaw JSONL via fsnotify, including deleted/reset files
- Timezone: system default

## License

MIT
