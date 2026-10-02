# nostr-relay

A private, lightweight Nostr relay written in Go, built for
encrypted 1-on-1 and group messaging. Designed as the relay
backend for an Android-based WhatsApp alternative: only you and
the people you authorize can read or write, and the whole thing
runs comfortably inside a 1 GB RAM VPS.

## Overview & Features

- **Private by default.** A hex pubkey whitelist gates both
  reads (`RejectFilter`) and writes (`RejectEvent`). Strangers
  cannot even see that your relay exists.
- **Mandatory NIP-42 authentication.** Every client must answer
  an `AUTH` challenge before doing anything useful. Connections
  that never authenticate, or authenticate with a pubkey that is
  not on the whitelist, are rejected with a clear, human-readable
  `NOTICE` message instead of a silent failure.
- **Persistent storage with embedded SQLite.** Events are stored
  on disk via `github.com/fiatjaf/eventstore/sqlite3` - no
  external database server, no extra memory, trivial backups
  (copy one file).
- **Low resource footprint.** A single small Go binary plus one
  SQLite file. Well suited to the Oracle Cloud Free Tier
  (VM.Standard.E2.1.Micro: 1 OCPU, 1 GB RAM) or any
  resource-constrained VPS.
- **Clean rejection policy.** Non-authenticated and
  non-whitelisted clients receive explicit notices such as:
  `restricted: auth required`, `restricted: pubkey not allowed`,
  so client developers can diagnose access problems instantly.

## Architecture & Tech Stack

```
Android client (NIP-42 capable Nostr client)
        |
        | wss:// / ws://  (WebSocket, NIP-01 protocol)
        v
+-----------------------------------------+
|  nostr-relay (Go, khatru framework)     |
|                                         |
|  - NIP-42 auth middleware (challenge,  |
|    verify kind-22242 signed response,   |
|    verify "relay" tag == Service URL)   |
|  - Whitelist policies:                 |
|      RejectEvent  -> gate WRITE (EVENT)|
|      RejectFilter -> gate READ  (REQ)  |
|  - NIP-11 relay information document   |
+-----------------------------------------+
        |
        v
+-----------------------------------------+
|  SQLite (fiatjaf/eventstore/sqlite3)    |
|  - single embedded .db file on disk     |
|  - CGO_ENABLED=1 build                  |
+-----------------------------------------+
```

Component breakdown:

| Component | Role |
|---|---|
| `khatru` | Relay framework: WebSocket handling, NIP-01 REQ/EVENT/CLOSE plumbing, middleware chain (RejectEvent, RejectFilter), NIP-42 helpers |
| `eventstore/sqlite3` | Embedded SQLite backend implementing khatru's storage interface (SaveEvent, QueryEvents, DeleteEvent) |
| NIP-42 auth flow | On connect the relay sends `["AUTH", <challenge>]`; the client must reply with a signed kind-22242 event containing the challenge and the relay URL; the relay verifies the signature and only then marks the connection authenticated |
| Whitelist | A list of allowed pubkeys in hex format; both the event-write path and the subscription path check the authenticated pubkey against it |

## Prerequisites

For local development:

- **Go 1.25 or newer** (`go version`)
- **A C compiler** (CGO is required by the SQLite driver):
  - Linux: `gcc` (Debian/Ubuntu: `sudo apt install build-essential`)
  - macOS: Xcode Command Line Tools (`xcode-select --install`)
  - Windows: MSYS2/MinGW-w64 or TDM-GCC
- **Docker** (only if you plan to build/run the container locally)
- (Optional) **websocat** or a Nostr debug client for manual testing

## Local Development Setup

```bash
# 1. Clone the repository
git clone <REPO_URL> nostr-relay
cd nostr-relay

# 2. Fetch dependencies
go get github.com/fiatjaf/khatru
go get github.com/fiatjaf/eventstore/sqlite3
go mod tidy

# 3. Enable CGO (required for the SQLite driver)
export CGO_ENABLED=1

# 4. Run the relay
go run main.go
```

The relay now listens on:

```
ws://localhost:3334
```

### Connecting from the Android Emulator

The emulator cannot use `localhost` to reach your development
machine. Point the Android client at:

```
ws://10.0.2.2:3334
```

(`10.0.2.2` is the Android emulator's alias for the host
machine's loopback interface.)

### Quick smoke test

With `websocat`:

```bash
websocat ws://localhost:3334
```

You should immediately receive the NIP-42 auth challenge:

```json
["AUTH", "0beec7b5ea3f0fdbc95d0dd47f3c5bc275da8a33"]
```

Any attempt to REQ or publish an EVENT before authenticating
returns a NOTICE such as `restricted: auth required`.

## Configuration

All configuration is provided through environment variables:

| Variable | Purpose | Example | Default |
|---|---|---|---|
| `PORT` | WebSocket listen port | `3334` | `3334` |
| `RELAY_URL` | Canonical service URL. Must match the `relay` tag clients place in their kind-22242 auth event, and is published in the NIP-11 document | `wss://relay.example.com` | `ws://localhost:3334` |
| `ALLOWED_PUBKEYS` | Comma-separated hex pubkeys allowed to read and write | `32e1827635450c39954...8b1c9` | (empty = nobody; build fails closed) |
| `DB_PATH` | SQLite database file location | `./nostr.db` | `./nostr.db` |

Example:

```bash
export RELAY_URL="ws://localhost:3334"
export ALLOWED_PUBKEYS="32e1827635450c3995442948f83...9b0f3b,5c7ed...e2a1"
go run main.go
```

Security notes:

- Pubkeys are 64-character hex strings. `npub...` bech32
  values are also accepted and are decoded to hex automatically;
  hex is the canonical form.
- If `ALLOWED_PUBKEYS` is empty the relay starts but rejects
  everyone: it fails closed, never open.
- Never expose the SQLite file; it contains all message events.

### SQLite tuning (recommended)

The driver (`mattn/go-sqlite3`, via `eventstore/sqlite3`) runs with
SQLite defaults unless you pass DSN parameters through `DB_PATH`.
For a small private relay under concurrent message writes, set a busy
timeout and enable WAL so concurrent access degrades gracefully
instead of failing with `database is locked` (SQLITE_BUSY):

```bash
export DB_PATH="file:/data/nostr.db?_busy_timeout=5000&_journal_mode=WAL&_synchronous=NORMAL"
```

Notes:

- The `file:` prefix form is required when passing DSN parameters.
- WAL mode leaves small `-wal`/`-shm` sidecar files next to the
  database; they are checkpointed automatically and are safe to
  include in your backup only together with the main file.
- If you prefer zero configuration, the plain path (default) also
  works; concurrency is simply lower under simultaneous writes.

## Docker & VPS Deployment

### Dockerfile (multi-stage, small final image)

```dockerfile
# ---- build stage ----
# go.mod requires the Go 1.25 toolchain (pulled in by khatru and
# go-nostr); build on a matching image so the build never tries to
# download a toolchain inside the container.
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache build-base

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=1 GOOS=linux go build -ldflags="-s -w" -o /nostr-relay .

# ---- runtime stage ----
FROM alpine:3.20

RUN apk add --no-cache ca-certificates && \
    adduser -D -H -u 1000 relay

USER relay
WORKDIR /home/relay

COPY --from=builder /nostr-relay /usr/local/bin/nostr-relay

VOLUME ["/home/relay/data"]
EXPOSE 3334

ENTRYPOINT ["nostr-relay"]
```

The multi-stage build keeps only the compiled binary in the
final image (typically under 20 MB), with the SQLite database
persisted through the `/home/relay/data` volume.

### Build and run locally

```bash
docker build -t nostr-relay .

docker run -d \
  --name nostr-relay \
  --restart unless-stopped \
  --memory 256m \
  -p 3334:3334 \
  -e RELAY_URL="wss://relay.example.com" \
  -e ALLOWED_PUBKEYS="32e182...9b0f3b" \
  -v relay-data:/home/relay/data \
  nostr-relay
```

### Deploying on a 1 GB RAM VPS (Oracle Cloud Free Tier)

1. Provision the instance (VM.Standard.E2.1.Micro, Ubuntu or
   any Linux image) and note its public IP.

2. Open the port in BOTH places:
   - Oracle Cloud console: Security List / NSG ingress rule for
     TCP 3334 (or 443 behind a reverse proxy).
   - Host firewall: `sudo ufw allow 3334/tcp`.

3. Install Docker:

   ```bash
   curl -fsSL https://get.docker.com | sh
   ```

4. Copy the project (or pull the image) and run the container
   as shown above, binding the database volume to a host
   directory for easy backup:

   ```bash
   -v /opt/nostr-relay/data:/home/relay/data
   ```

5. For production, terminate TLS with a reverse proxy
   (Caddy is the lightest option and handles certificates
   automatically):

   ```caddy
   relay.example.com {
       reverse_proxy localhost:3334
   }
   ```

   and set `RELAY_URL=wss://relay.example.com`.

6. Back up by copying the SQLite file:

   ```bash
   sqlite3 /opt/nostr-relay/data/nostr.db ".backup '/backup/nostr-$(date +%F).db'"
   ```

Resource guidance for the 1 GB box: cap container memory
(`--memory 256m` is plenty), avoid swap thrash by not hosting
anything else on the instance, and prefer the Alpine-based
image.

## Nostr NIPs Supported

| NIP | Status | Notes |
|---|---|---|
| **NIP-01** | Supported | Core protocol: `EVENT`, `REQ`, `CLOSE`, `NOTICE`, plus NIP-01-compliant event validation and subscription filters |
| **NIP-11** | Supported | Relay information document served over plain HTTP `GET /` (name, description, pubkey whitelist policy, supported NIPs) |
| **NIP-42** | Supported | Mandatory `AUTH` challenge on connect; kind-22242 signed auth events verified against the relay URL; unauthenticated reads/writes rejected |

Notes:

- The relay is intentionally minimal: ephemeral messages
  (NIP-04/NIP-17 style chat events) and gift wraps are stored
  and forwarded, but there are no plans for NIP-05, NIP-26, or
  paid relay features.
- Client applications MUST implement NIP-42 to use this relay.
- Any future NIP support will be listed here first.

## License

Private project. All rights reserved.
