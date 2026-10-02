# nostr-relay Review Remediation Report

**Project:** nostr-relay - private Nostr relay (Go, khatru + sqlite3)
**Task:** apply the owner-approved remediation queue from
CODE_REVIEW_REPORT.md (findings F1, F3, F4; F2 accepted as-is per the
recommendation), then run a full regression suite.
**Date:** October 2, 2026

## 0. Pre-Change Verification (per standing instructions)

- Working directory verified clean before changes: exactly the six
  files from the previous steps, no stray artifacts; main.go md5
  matched the code-review baseline (0c4d6e19...).
- Environment check: Go 1.25.0 toolchain present; no relay processes
  running; CGO toolchain (gcc) available.
- No GitHub push performed: no repository URL has been provided for
  nostr-relay and the standing instruction forbids pushing until
  explicitly instructed.

## 1. F1 (HIGH): Kind-5 Deletion Gate - FIXED

Change: registered the auth/whitelist gate as an
OverwriteDeletionOutcome closure, which khatru consults inside
handleDeleteRequest BEFORE DeleteEvent executes. The closure
re-applies the protocol's author-match rule (only the event's author
may delete) and additionally requires the requesting CONNECTION to
pass the same gate used for reads and writes.

Empirical regression proof (live relay, whitelisted key K):

- Before fix (previous report): unauthenticated kind-5 by K deleted
  the stored event while the relay answered "restricted: auth
  required".
- After fix: unauthenticated kind-5 by K -> ["OK", id, false,
  "blocked: restricted: auth required"] and the event REMAINED in
  SQLite (verified by authenticated query).
- Functional preservation: an authenticated, whitelisted, author
  kind-5 by K -> ["OK", id, true, ""] and the event WAS deleted.

## 2. F3 (MEDIUM): WebSocket Drain on Shutdown - FIXED

Change: replaced the custom http.Server with the khatru-native
lifecycle: relay.Start("", port, started) now runs the listener (the
relay itself remains the handler, so NIP-11 and websocket upgrades
are unaffected), and on SIGINT/SIGTERM main() calls relay.Shutdown(
ctx), which stops the listener, sends a close control frame to every
live websocket and cancels their contexts. db.Close() was moved from
a defer to an explicit call AFTER the drain, making the ordering
legible and guaranteed: drain -> close database -> exit.

Empirical proof: with an authenticated client holding an open
websocket, SIGTERM produced:

- Client side: immediate clean close frame
  ("received close frame: StatusNoStatusRcvd") - the client was
  notified, not hung and not hard-cut mid-read.
- Relay logs: "received terminated, shutting down gracefully" ->
  "storage: database closed cleanly" -> "shutdown complete".

## 3. F4 (MEDIUM): SQLite DSN Documentation - DONE

Added a "SQLite tuning (recommended)" section to README.md
documenting the WAL/busy_timeout DSN form for DB_PATH
(file:...?_busy_timeout=5000&_journal_mode=WAL&_synchronous=NORMAL),
why it prevents SQLITE_BUSY under concurrent writes, and the WAL
sidecar-file backup note. Zero code change, as planned.

Documentation correction found while editing: README claimed npub1...
values were NOT accepted, but the implementation (and previous
report) accepts and decodes them; the README note was corrected to
match actual behavior (hex remains the canonical form).

## 4. F2 (MEDIUM): event.PubKey Policy - Owner Decision Recorded

Per the approved recommendation: the connection-level whitelist gate
is KEPT as-is (spec-compliant, required by NIP-17-style clients that
publish with derived keys). No per-event author enforcement added.
Impersonation of other users remains impossible (every event must
carry a valid signature of its own pubkey). If the Android client
ever settles on one stable identity key per user, the strict
3-line variant remains documented in the code review report.

## 5. Full Regression Matrix (all on the rebuilt binary)

- Build: CGO_ENABLED=1 go build OK; go vet clean; gofmt clean;
  main.go is 303 lines, still a single file.
- Scenario A (unauthenticated): EVENT and REQ both rejected with the
  exact "restricted: auth required" messages.
- Scenario B (authenticated + whitelisted): publish OK true; REQ
  returned the stored event + EOSE.
- Scenario C (authenticated, not whitelisted): rejected with
  "restricted: pubkey not allowed" on both write and read.
- Delete regression: see Section 1 (bypass closed, function intact).
- Drain test: see Section 2.
- Persistence: after SIGTERM + restart on the same DB, the
  scenario-B event was still served.
- Fail-closed: empty ALLOWED_PUBKEYS -> loud warning; all reads and
  writes rejected.

## 6. Files Changed

- main.go: +1 gate closure (OverwriteDeletionOutcome), server
  lifecycle rewritten to relay.Start/relay.Shutdown, db.Close moved
  after the drain. No other logic touched.
- README.md: SQLite tuning section + npub documentation correction.
- REMEDIATION_REPORT.md: this report.

## 7. Compliance Statement

- Only the owner-approved queue items were implemented (F1, F3, F4);
  F2 recorded as "keep current behavior" per the approval.
- No scope expansion; no new files beyond the report.
- No GitHub push (awaiting explicit instruction and the repository
  URL for nostr-relay).

## 8. Next Steps

1. Owner provides the GitHub repository URL for nostr-relay.
2. On explicit instruction: init/commit/push (code, go.mod, go.sum,
   README, all three reports).
3. Optional later hardening (not approved yet): connection rate
   limiting, COUNT wiring.

**END OF REMEDIATION REPORT**
