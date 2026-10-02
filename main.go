// nostr-relay: a private, lightweight Nostr relay for encrypted
// 1-on-1 and group messaging.
//
// Security model:
//   - NIP-42 authentication is mandatory on every connection.
//   - Reads and writes are gated by a hex pubkey whitelist.
//   - With an empty whitelist the relay fails closed: everyone
//     is rejected, nobody is admitted by accident.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/fiatjaf/eventstore/sqlite3"
	"github.com/fiatjaf/khatru"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip11"
	"github.com/nbd-wtf/go-nostr/nip19"
)

// ---------------------------------------------------------------------------
// Configuration (environment variables)
// ---------------------------------------------------------------------------

// config holds every runtime setting of the relay.
type config struct {
	port           string
	relayURL       string
	allowedPubkeys map[string]struct{}
	dbPath         string
}

// loadConfig reads and strictly validates the environment. Any invalid
// value is a fatal configuration error: the relay must not start with
// a silently wrong port or a corrupted whitelist.
func loadConfig() config {
	cfg := config{
		port:     envOrDefault("PORT", "3334"),
		relayURL: envOrDefault("RELAY_URL", "ws://localhost:3334"),
		dbPath:   envOrDefault("DB_PATH", "./nostr.db"),
	}

	if err := validatePort(cfg.port); err != nil {
		log.Fatalf("config: invalid PORT %q: %v", cfg.port, err)
	}
	if !strings.HasPrefix(cfg.relayURL, "ws://") && !strings.HasPrefix(cfg.relayURL, "wss://") {
		log.Fatalf("config: invalid RELAY_URL %q: must start with ws:// or wss://", cfg.relayURL)
	}

	cfg.allowedPubkeys = parseAllowedPubkeys(os.Getenv("ALLOWED_PUBKEYS"))
	if len(cfg.allowedPubkeys) == 0 {
		// Fail closed, never open: the relay starts and runs, but
		// rejects every read and write until a whitelist is provided.
		log.Printf("config: WARNING: ALLOWED_PUBKEYS is empty; " +
			"failing closed (all reads and writes will be rejected)")
	}

	return cfg
}

// envOrDefault returns the environment variable value or a default.
func envOrDefault(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// validatePort ensures PORT is a valid TCP port number.
func validatePort(port string) error {
	n, err := strconv.Atoi(port)
	if err != nil {
		return errors.New("not a number")
	}
	if n < 1 || n > 65535 {
		return errors.New("out of range 1-65535")
	}
	return nil
}

// parseAllowedPubkeys builds the whitelist lookup set from a
// comma-separated list of 64-character hex public keys. Entries may
// also be given as bech32 npub... values for convenience and are
// converted to hex. Invalid entries are logged and skipped.
func parseAllowedPubkeys(raw string) map[string]struct{} {
	allowed := make(map[string]struct{})
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" {
			continue
		}

		// Accept npub... values by decoding them to hex.
		if strings.HasPrefix(entry, "npub1") {
			_, data, err := nip19.Decode(entry)
			if err != nil {
				log.Printf("config: skipping invalid npub in ALLOWED_PUBKEYS: %q: %v", entry, err)
				continue
			}
			if pub, ok := data.(string); ok {
				entry = strings.ToLower(pub)
			} else {
				log.Printf("config: skipping non-npub value in ALLOWED_PUBKEYS: %q", entry)
				continue
			}
		}

		if !isHexPubkey(entry) {
			log.Printf("config: skipping invalid pubkey in ALLOWED_PUBKEYS "+
				"(expected 64 hex characters): %q", entry)
			continue
		}
		allowed[entry] = struct{}{}
	}
	return allowed
}

// isHexPubkey reports whether s is a valid 64-character hex pubkey.
func isHexPubkey(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Access control (NIP-42 + whitelist)
// ---------------------------------------------------------------------------

// gateAccess is the shared policy applied to both event writes
// (RejectEvent) and queries/subscriptions (RejectFilter).
//
// Returns a rejection pair when the connection is not authenticated
// or the authenticated pubkey is not whitelisted.
func gateAccess(ctx context.Context, allowed map[string]struct{}) (bool, string) {
	pubkey := khatru.GetAuthed(ctx)

	// Not authenticated (no valid NIP-42 kind-22242 exchange yet).
	if pubkey == "" {
		return true, "restricted: auth required"
	}

	// Authenticated but not whitelisted.
	if _, ok := allowed[pubkey]; !ok {
		return true, "restricted: pubkey not allowed"
	}

	return false, ""
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	log.SetPrefix("[nostr-relay] ")

	cfg := loadConfig()

	// --- storage ---------------------------------------------------------
	db := &sqlite3.SQLite3Backend{
		DatabaseURL: cfg.dbPath,
	}
	if err := db.Init(); err != nil {
		log.Fatalf("storage: failed to initialize SQLite at %q: %v", cfg.dbPath, err)
	}
	// NOTE: the database is closed explicitly at the end of main(),
	// only after every websocket connection has been drained.
	log.Printf("storage: SQLite ready at %q", cfg.dbPath)

	// --- relay + NIP-11 information document ------------------------------
	relay := khatru.NewRelay()
	relay.ServiceURL = cfg.relayURL // used by khatru for NIP-42 "relay" tag verification

	relay.Info.Name = "nostr-relay"
	relay.Info.Description =
		"Private relay for encrypted 1-on-1 and group messaging. " +
			"NIP-42 authentication and a pubkey whitelist are required."
	relay.Info.SupportedNIPs = []any{1, 11, 42}
	relay.Info.Software = "https://github.com/fiatjaf/khatru"
	relay.Info.Limitation = &nip11.RelayLimitationDocument{
		AuthRequired:     true,
		RestrictedWrites: true,
	}

	// --- NIP-42: challenge every new connection immediately ----------------
	relay.OnConnect = append(relay.OnConnect, func(ctx context.Context) {
		khatru.RequestAuth(ctx) // sends ["AUTH", <challenge>]
	})

	// --- wire storage hooks ------------------------------------------------
	relay.StoreEvent = append(relay.StoreEvent, db.SaveEvent)
	relay.QueryEvents = append(relay.QueryEvents, db.QueryEvents)
	relay.DeleteEvent = append(relay.DeleteEvent, db.DeleteEvent)

	// --- gate writes (EVENT) -----------------------------------------------
	relay.RejectEvent = append(relay.RejectEvent,
		func(ctx context.Context, event *nostr.Event) (bool, string) {
			return gateAccess(ctx, cfg.allowedPubkeys)
		},
	)

	// --- gate reads (REQ / subscriptions) -----------------------------------
	relay.RejectFilter = append(relay.RejectFilter,
		func(ctx context.Context, filter nostr.Filter) (bool, string) {
			return gateAccess(ctx, cfg.allowedPubkeys)
		},
	)

	// --- gate deletions (kind 5) --------------------------------------------
	//
	// khatru processes kind-5 deletion requests BEFORE the RejectEvent
	// hooks: handleDeleteRequest runs first and calls DeleteEvent
	// directly, so the gates above never see a deletion that succeeds.
	// khatru consults OverwriteDeletionOutcome inside
	// handleDeleteRequest, before any deletion is executed - and it
	// OVERWRITES the natural author-match decision, so this closure
	// must re-apply that rule itself and combine it with the gate.
	relay.OverwriteDeletionOutcome = append(relay.OverwriteDeletionOutcome,
		func(ctx context.Context, target *nostr.Event, deletion *nostr.Event) (bool, string) {
			// keep the protocol's rule: only the author may delete
			if target.PubKey != deletion.PubKey {
				return false, "you are not the author of this event"
			}
			// the requesting connection must also pass the auth gate
			if reject, msg := gateAccess(ctx, cfg.allowedPubkeys); reject {
				return false, msg
			}
			return true, ""
		},
	)

	// --- HTTP/WebSocket server (khatru-native) with graceful drain ----------
	//
	// The server is started through relay.Start (not our own
	// http.Server) on purpose: khatru's Shutdown() can then stop the
	// http listener AND send a close control frame to every live
	// websocket, cancelling their contexts. Hijacked websocket
	// connections are invisible to http.Server.Shutdown, so this is
	// the only way to guarantee no client can still touch the
	// database once we close it below.
	portNum, err := strconv.Atoi(cfg.port)
	if err != nil {
		log.Fatalf("config: PORT %q not numeric: %v", cfg.port, err)
	}

	started := make(chan bool, 1)
	serverErr := make(chan error, 1)
	go func() {
		log.Printf("listening on :%s (relay url: %s, %d whitelisted pubkey(s))",
			cfg.port, cfg.relayURL, len(cfg.allowedPubkeys))
		if err := relay.Start("", portNum, started); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	// Wait until the listener is up or the server fails to start.
	select {
	case <-started:
	case err := <-serverErr:
		log.Fatalf("http: server failed to start: %v", err)
	}

	// Block until SIGINT/SIGTERM or a fatal server error.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		log.Fatalf("http: server failed: %v", err)
	case sig := <-sigCh:
		log.Printf("received %v, shutting down gracefully", sig)
	}

	// Drain: stop accepting new connections, then close every live
	// websocket with a proper close frame and cancel its context.
	drainCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	relay.Shutdown(drainCtx)

	// Only after the drain is it safe to close the SQLite store.
	db.Close()
	log.Printf("storage: database closed cleanly at %q", cfg.dbPath)
	log.Printf("shutdown complete")
}
