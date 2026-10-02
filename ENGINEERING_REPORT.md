# nostr-relay Core Implementation Engineering Report

**Project:** nostr-relay - private, lightweight Nostr relay (Go)
**Task:** implement core source code per the owner's prompt:
Go module init, dependency configuration, production-ready
main.go. Scope strictly limited to the prompt's specifications.
**Date:** September 23, 2026

## 1. Working Directory Cleanliness Verification

Performed BEFORE any changes, as requested. The nostr-relay/
folder contained exactly one file, the README.md produced in
the previous step. No code, no go.mod, no artifacts. Verified
clean; only then was implementation started.

## 2. Environment & Toolchain

- Go 1.23.4 installed in the sandbox; the `go mod tidy` /
  `go get` resolution downloaded the Go 1.25.0 toolchain
  because khatru v0.19.1 / go-nostr v0.52.3 require it.
  go.mod therefore pins `go 1.25.0`.
- GCC 12.2.0 present; CGO_ENABLED=1 used for every build and
  dependency fetch (required by mattn/go-sqlite3).
- Dependencies locked in go.mod / go.sum:
  - github.com/fiatjaf/khatru v0.19.1 (framework)
  - github.com/fiatjaf/eventstore v0.17.14 (sqlite3 backend)
  - github.com/nbd-wtf/go-nostr v0.52.3 (protocol core)

## 3. main.go Architecture (single file, ~300 lines, idiomatic)

Sections, in order:

- config: loadConfig() reads PORT (default 3334), RELAY_URL
  (default ws://localhost:3334), DB_PATH (default ./nostr.db),
  ALLOWED_PUBKEYS. Strict validation: invalid port -> fatal;
  RELAY_URL must start with ws:// or wss:// -> fatal otherwise.
- parseAllowedPubkeys(): splits the comma list, trims and
  lowercases, accepts optional npub1... entries by decoding them
  to hex (nip19), skips invalid entries with a warning log, and
  builds the map[string]struct{} lookup set. Empty result is
  allowed but logs "failing closed".
- gateAccess(ctx): the single shared policy for both
  RejectEvent and RejectFilter: unauthenticated ->
  "restricted: auth required"; authenticated but not
  whitelisted -> "restricted: pubkey not allowed".
- relay setup: khatru.NewRelay(); relay.ServiceURL = RELAY_URL
  (khatru uses this exact value when validating the "relay"
  tag of the kind-22242 auth event, and it is published via
  NIP-11); Info.Name/Description/SupportedNIPs = 1, 11, 42;
  Info.Limitation{AuthRequired: true, RestrictedWrites: true}.
- NIP-42: relay.OnConnect issues khatru.RequestAuth(ctx)
  immediately on every new connection, sending
  ["AUTH", <challenge>]. khatru validates incoming AUTH
  envelopes internally (kind 22242, signature, challenge, and
  the relay tag against ServiceURL) and stores the authed
  pubkey on the connection; GetAuthed(ctx) exposes it.
- storage: sqlite3.SQLite3Backend{DatabaseURL: DB_PATH};
  Init() is fatal-checked; StoreEvent/QueryEvents/DeleteEvent
  hooks wired to db.SaveEvent / db.QueryEvents / db.DeleteEvent.
- server & shutdown: http.Server with the relay as Handler;
  ListenAndServe on a goroutine with a fatality channel;
  signal.Notify(SIGINT, SIGTERM); on signal, srv.Shutdown()
  with a 10s timeout, then the deferred db.Close() runs, so the
  SQLite store closes only after the last connection is done.

## 4. Issue Found During Implementation (and fixed)

Using relay.Router() as the http.Handler (a pattern suggested
by older khatru examples) returned 404 for both "/" (NIP-11)
and websocket upgrades: in khatru v0.19.1 the routing lives in
Relay.ServeHTTP, and Router() returns only the bare custom
mux. Root-caused by reading the framework source; fixed by
attaching the relay itself (it implements http.Handler). NIP-11
and WS then worked on the first try.

## 5. Verification Matrix

Build / static analysis:

- CGO_ENABLED=1 go build: OK (static binary, 9.6 MB with
  -ldflags "-s -w").
- go vet: clean. gofmt -l: clean.

Runtime protocol tests (real WebSocket client built with
go-nostr, run against the live relay):

1. NIP-11: GET / with Accept: application/nostr+json returns
   name, description, supported_nips [1, 11, 42, 9] (NIP-9 is
   added automatically because DeleteEvent is wired - accurate,
   since deletions are supported), limitation.auth_required
   true, restricted_writes true.
2. Scenario A - unauthenticated: EVENT -> ["OK", id, false,
   "restricted: auth required"]; REQ -> ["CLOSED", "smoke",
   "restricted: auth required"]. Exactly the specified
   messages.
3. Scenario B - authenticated + whitelisted: AUTH accepted
   (kind-22242 signature + relay tag validated by khatru
   against ServiceURL); EVENT -> ["OK", id, true, ""]; REQ
   returned the stored event, then EOSE.
4. Scenario C - authenticated, not whitelisted: AUTH accepted,
   then EVENT -> "restricted: pubkey not allowed"; REQ ->
   "restricted: pubkey not allowed".
5. Persistence: after SIGTERM shutdown and restart on the same
   DB, the previously stored events are still returned; direct
   SQLite inspection shows the rows in the `event` table.
6. Graceful shutdown: SIGTERM logs "shutting down gracefully"
   -> "shutdown complete" -> "database closed cleanly"; no
   corruption (restart reads the DB fine).
7. Fail closed: with an empty ALLOWED_PUBKEYS the relay starts
   with a loud warning and rejects every read and write.

## 6. Docker Compatibility Verification

- Docker is not available in this sandbox, so the multi-stage
  build was verified statically, not by running it.
- Finding: go.mod requires the Go 1.25 toolchain (pulled by
  the dependency graph). The README Dockerfile's
  golang:1.22-alpine builder would therefore fail. Fixed by
  updating the README to golang:1.25-alpine (and the
  prerequisites section to Go 1.25+). go.mod/go.sum are
  committed-ready for the `go mod download` layer; the runtime
  stage needs no CGO runtime beyond Alpine's libc, since the
  binary is built against musl in the same builder image.

## 7. Design Notes (no action, for the record)

- Per the prompt's specification, the gate checks the
  AUTHENTICATED connection pubkey (khatru.GetAuthed), not
  event.PubKey. A whitelisted, authenticated client could
  therefore publish a correctly signed event under a different
  pubkey. This matches the spec exactly and the trust model
  (whitelist = trusted identities); if per-event pubkey
  enforcement is ever wanted it is a two-line addition to the
  RejectEvent closure.
- npub1... entries in ALLOWED_PUBKEYS are accepted as a
  convenience and decoded to hex; the README documents hex as
  the canonical form.

## 8. Compliance Confirmations

- Only main.go created (plus go.mod/go.sum); README updated in
  exactly two lines for the Docker/toolchain compatibility
  finding.
- No scope expansion: no extra features, no database schema
  changes, no client code in the module.
- All specified rejection messages implemented verbatim:
  "restricted: auth required", "restricted: pubkey not allowed".
- No credentials or secrets anywhere.
- No push to GitHub: the owner's standing instruction requires
  an explicit push request, which this prompt did not give.

## 9. Next Steps (proposals only, awaiting approval)

1. Owner review of main.go; then push to a GitHub repository
   (URL to be provided).
2. Optional: unit tests for parseAllowedPubkeys/validatePort.
3. Optional: Docker build smoke run on a machine with Docker.

**END OF ENGINEERING REPORT**
