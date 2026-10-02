# nostr-relay Critical Code Review Report

**Project:** nostr-relay - private Nostr relay (Go, khatru + eventstore/sqlite3)
**Task:** comprehensive critical code review of main.go / go.mod / go.sum
before finalization. READ-ONLY: no project files were modified, no source
code generated, no push performed. All hypotheses were verified BOTH by
framework source audit (khatru v0.19.1, eventstore v0.17.14, go-nostr
v0.52.3) AND by live empirical probes (test harness kept outside the
project tree, in /tmp).
**Date:** September 23, 2026

## 0. Working Directory Cleanliness Verification

Verified BEFORE starting: nostr-relay/ contains exactly the six files
produced by the previous step (README.md, ENGINEERING_REPORT.md, go.mod,
go.sum, main.go, compiled binary). main.go md5 verified identical before
and after this review: no modification occurred.

## 1. Findings Summary

| ID | Severity | Finding |
|----|----------|---------|
| F1 | HIGH | Kind-5 deletion executes BEFORE the auth gate (bypass confirmed) |
| F2 | MEDIUM | Writes are gated by connection auth key, not by event.PubKey (Section 7 of the previous report) |
| F3 | MEDIUM | Shutdown sequence does not drain live WebSocket connections before SQLite close |
| F4 | MEDIUM | SQLite backend runs with default DSN: no busy_timeout / WAL configured |
| F5 | LOW | COUNT requests: not wired, correctly answered as unsupported/gated - no leak, but worth documenting |
| F6 | LOW | No connection-level rate limiting (DoS surface on a public port) |
| F7 | PASS | Fail-closed behavior verified: missing/empty ALLOWED_PUBKEYS rejects everything |
| F8 | PASS | Env parsing and npub decoding: no panic paths found |
| F9 | PASS | NIP-42 flow verified end to end |
| F10 | PASS | Rejection messages exactly as specified; NIP-11 document accurate |

## 2. F1 (HIGH): Kind-5 Deletion Bypasses the Auth Gate - Empirically Confirmed

Evidence (live probe against the running relay, whitelisted key K):

1. Authenticated as K, published event E (kind 1). Relay answered
   ["OK", E.id, true, ""].
2. Opened a NEW connection and did NOT authenticate. Sent a kind-5
   deletion request signed by K with tag ["e", E.id].
3. Relay answered ["OK", ..., false, "restricted: auth required"] -
   LOOKS rejected.
4. Reconnected, authenticated as K, queried by id: EVENT GONE. The
   unauthenticated connection had actually deleted E from SQLite.

Root cause (khatru handlers.go, EVENT envelope path): for kind 5,
handleDeleteRequest(ctx, evt) runs BEFORE handleNormal() (which is where
our RejectEvent gate is consulted). handleDeleteRequest itself queries
the store with an internal-call context (bypassing RejectFilter by
design), checks target.PubKey == evt.PubKey, and calls DeleteEvent. Only
afterwards does the gate run - by then the deletion already happened.
The OK reply comes from the gate, which is why the response falsely
suggests the write was rejected.

Exploitability: requires the private key that signed the original
event (only its author can forge the kind-5 signature). So a stranger
cannot delete someone else's messages. But an owner who was REMOVED
from the whitelist, or a revoked device key, can still delete its own
previously stored events while being denied everywhere else - and the
relay logs/replies give no hint that a deletion occurred.

Recommended fix (when code changes are approved): register the gate as
an OverwriteDeletionOutcome closure - it is consulted inside
handleDeleteRequest before DeleteEvent. Because it OVERWRITES the
natural author-match decision, the closure must return
(target.PubKey == evt.PubKey) AND gateAccess(ctx, allowed); otherwise
(false, "restricted: auth required" / "restricted: pubkey not
allowed"). Roughly 10 lines; the current main.go structure supports it
without any other change.

## 3. F2 (MEDIUM): Section 7 Assessment - Auth Key vs event.PubKey

Empirical evidence (from the previous step's scenario B): a connection
authenticated as whitelisted key K successfully published a kind-1
event signed by a different, non-whitelisted key K2. The relay stored
and rebroadcast it.

Precise risk analysis (correcting the report's wording):

- Impersonation of OTHER users is NOT possible: every event must carry
  a valid signature of its own pubkey, so nobody can publish under a
  key they do not control. The relay identity model is intact.
- The real gap: whitelist enforcement applies to the AUTH key only.
  A single whitelisted auth key can publish from arbitrarily many
  non-whitelisted author keys (its other devices/derived keys). For
  NIP-17-style messaging this is actually REQUIRED behavior - seal and
  giftwrap events may be signed by derived keys. Strictly enforcing
  event.PubKey == GetAuthed(ctx) would break such clients.
- Residual risks: storage abuse multiplier (one whitelist entry, many
  author keys), and event authors are not individually auditable
  against the whitelist.

Recommendation: keep the connection-level gate (spec-compliant), and
add event.PubKey-in-whitelist enforcement ONLY if the Android client
model uses one stable identity key per user (then it is a 3-line
addition to the RejectEvent closure). Decision belongs to the owner.

## 4. F3 (MEDIUM): Shutdown Does Not Drain WebSocket Connections

Go's http.Server.Shutdown does not wait for hijacked connections, and
all Nostr WebSockets are hijacked. So the current sequence
(srv.Shutdown -> deferred db.Close) closes the database while live
WebSocket clients may still be mid-conversation.

Mitigating facts (why this is MEDIUM, not HIGH):

- database/sql's DB.Close blocks until all queries that have started
  processing finish, so SQLite integrity is preserved (no corruption).
- The process exits immediately after, which closes the sockets.
- SIGTERM/SIGINT handling itself is correct and was verified live:
  clean logs, clean DB close, intact DB on restart.

Framework fact: khatru DOES ship relay.Shutdown(ctx), which sends a
close control frame to every client and cancels their contexts - a
proper drain. BUT it calls rl.httpServer.Shutdown, and rl.httpServer
is only set by relay.Start(); with our own http.Server it is nil and
Go's Server.Shutdown has no nil-receiver check -> it would panic.

Recommended fix (when approved): adopt the khatru-native pattern:
start the server with relay.Start(host, port) on a goroutine, then on
SIGTERM call relay.Shutdown(ctx) (drains all WS clients properly, now
safe because rl.httpServer is set), then db.Close(). This also
simplifies main.go. Until then, the honest wording is: the shutdown
is graceful for the DATABASE, abrupt for live CLIENTS.

## 5. F4 (MEDIUM): SQLite DSN Defaults

eventstore's Init() calls sqlx.Connect("sqlite3", DatabaseURL) with no
busy_timeout, WAL, or pool tuning. Under concurrent writers, mattn/
go-sqlite3 in default rollback-journal mode returns SQLITE_BUSY
immediately ("database is locked") instead of waiting. For a small
private relay with a handful of users the practical impact is low, but
it is the single most likely source of intermittent write failures.

Recommended fix (when approved): pass DSN parameters through the
existing DB_PATH env var, e.g.
"file:/data/nostr.db?_busy_timeout=5000&_journal_mode=WAL&_synchronous=NORMAL"
- supported by mattn/go-sqlite3, zero code change (only README
documentation of the option). Optionally SetMaxOpenConns on the pool.

Positive resource notes for the 1 GB VPS target: eventstore defaults
cap queries (QueryLimit 100, IDs 500, authors 500, kinds 10); khatru
defaults cap message size at 512 KB and buffer sizes at 1 KB.

## 6. F5-F6 (LOW): COUNT and Connection Flooding

- COUNT: CountEvents is not wired, so count requests get CLOSED
  (verified live: "restricted: auth required" through the REQ gate)
  or "unsupported: NIP-45". No information leaks. If COUNT is ever
  needed, wire db.CountEvents AND the same gate into
  RejectCountFilter (never one without the other).
- No RejectConnection rate limiting is configured; every TCP
  connection receives an AUTH challenge. For a private relay the
  README already implies network-level protection (Oracle security
  lists); the cleanest hardening is to keep the port closed to the
  internet (WireGuard/allowlist) rather than add code.

## 7. Verified-Pass Items

- NIP-42: challenge issued on every connect (OnConnect ->
  RequestAuth); kind-22242 validation (signature, challenge, relay
  tag vs ServiceURL) is khatru's nip42.ValidateAuthEvent - source
  verified and live-verified in the previous step.
- Fail-closed: empty or missing ALLOWED_PUBKEYS -> warning logged,
  every read/write rejected (verified live). Map is built once and
  only read afterwards, so concurrent gate access is race-free.
- Env parsing: PORT range-checked; RELAY_URL scheme-checked; npub
  decode errors are handled without panic (fixed in the previous
  step, re-verified here); hex validation is strict after
  lowercasing.
- Ephemeral events go through the same RejectEvent gate (source
  verified: handleEphemeral consults rl.RejectEvent).
- Negentropy/NIP-77 is off by default and was never enabled - no
  sync bypass exists.
- Rejection messages byte-match the spec strings.
- go.mod/go.sum: two direct deps + khatru's graph, all pinned; no
  replace directives; Go 1.25 toolchain requirement consistent with
  the README's Dockerfile. Module name "nostr-relay" is fine for a
  private module; if it is ever imported by other Go code, renaming
  to github.com/<user>/nostr-relay is conventional.

## 8. Architectural Summary

Strengths: single-file clarity; strict config validation with
fail-closed default; one shared gate function for reads and writes;
correct handler wiring after the Router() pitfall; spec-exact
notices; sensible reliance on framework resource caps.

Weaknesses (all fixable in under ~40 total lines once approved): the
deletion path ordering (F1) is the one true enforcement hole; the
shutdown sequence is honest-but-abrupt for clients (F3); SQLite
concurrency tuning is default (F4); and the auth-key-vs-author-key
policy should be an explicit owner decision (F2).

## 9. Compliance Statement

- No project file was modified or created (only this report); main.go
  md5 unchanged before/after.
- The empirical test harness lives outside the project tree (/tmp).
- No push to GitHub: per standing instruction, awaiting explicit
  instruction and repository details from the owner.

## 10. Recommended Action Queue (awaiting owner approval)

1. F1: gate OverwriteDeletionOutcome (HIGH, ~10 lines).
2. F3: switch to relay.Start + relay.Shutdown drain pattern (~15 lines).
3. F4: document WAL/busy_timeout DSN in README (0 lines of code).
4. F2: owner decision on event.PubKey policy (0-3 lines depending).
5. Then: push to GitHub (repository URL needed from the owner).

**END OF CRITICAL CODE REVIEW REPORT - NO FILES MODIFIED**
