// BurnoutRadar MCP Daemon — main entry point.
//
// Wires together:
//   - analytics.Hasher        — ephemeral daily salt for HMAC user-ID hashing
//   - analytics.Pipeline      — ZKA processing engine (one per channel)
//   - store.ChannelStore      — thread-safe multi-channel registry
//   - db.DB                   — SQLite store for scalar metrics only
//   - api.Server              — HTTP server for Slack webhooks (:8080)
//   - mcp.MCPServer           — Model Context Protocol server (:8081)
//   - StartEvaluationTicker   — background worker that polls all channels
//
// ZKA GUARANTEE: Raw user IDs and message text never reach disk, stdout, or
// the MCP layer. Only final mathematical scalars are persisted or exposed.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"burnoutradar-mcp/analytics"
	"burnoutradar-mcp/api"
	"burnoutradar-mcp/db"
	"burnoutradar-mcp/mcp"
	"burnoutradar-mcp/store"
)

func main() {
	// ------------------------------------------------------------------ //
	// 1. Initialise the ZKA Hasher with a freshly generated daily salt.   //
	//    ZKA: The salt is created in RAM via crypto/rand — never written   //
	//    to disk or logged. Losing the process means losing the salt,      //
	//    making past hashes permanently non-reversible (forward secrecy).  //
	// ------------------------------------------------------------------ //
	hasher, err := analytics.NewHasher()
	if err != nil {
		log.Fatalf("main: failed to create ZKA hasher: %v", err)
	}
	log.Println("main: ZKA hasher initialised with ephemeral daily salt")

	// ------------------------------------------------------------------ //
	// 2. Build the multi-channel ChannelStore and register monitored       //
	//    channels from the MONITORED_CHANNELS environment variable.        //
	//                                                                       //
	//    Format: MONITORED_CHANNELS="C123:backend-team,C456:general"        //
	//    If no name is provided (e.g. "C123"), the channel ID is used.      //
	// ------------------------------------------------------------------ //
	channelStore := store.NewChannelStore()

	monitored := parseMonitoredChannels(os.Getenv("MONITORED_CHANNELS"))
	if len(monitored) == 0 {
		// Fallback: seed one demo channel so the worker has something to evaluate.
		log.Println("main: MONITORED_CHANNELS not set — using demo channel C_DEMO:demo-channel")
		monitored = []channelEntry{{ID: "C_DEMO", Name: "demo-channel"}}
	}
	for _, ch := range monitored {
		channelStore.Register(ch.ID, ch.Name, hasher, 100.0, 20.0, 14.0)
		log.Printf("main: registered channel %s (#%s)", ch.ID, ch.Name)
	}
	log.Printf("main: monitoring %d channel(s)", channelStore.Len())

	// ------------------------------------------------------------------ //
	// 3. Open the SQLite database.                                         //
	//    ZKA: Only scalar metrics are ever written here.                   //
	//    Default path: ./burnout-metrics.db (relative to binary location). //
	//    Override via BURNOUT_DB_PATH environment variable.                //
	// ------------------------------------------------------------------ //
	dbPath := os.Getenv("BURNOUT_DB_PATH")
	if dbPath == "" {
		// Use the directory of the binary as the default DB location.
		exe, err := os.Executable()
		if err != nil {
			log.Fatalf("main: failed to resolve executable path: %v", err)
		}
		dbPath = filepath.Join(filepath.Dir(exe), "burnout-metrics.db")
	}

	database, err := db.NewDB(dbPath)
	if err != nil {
		log.Fatalf("main: failed to open database at %q: %v", dbPath, err)
	}
	defer func() {
		if err := database.Close(); err != nil {
			log.Printf("main: error closing database: %v", err)
		}
	}()
	log.Printf("main: SQLite database opened at %s", dbPath)

	// ------------------------------------------------------------------ //
	// 4. Create the API server (Slack webhooks, flush trigger).            //
	//    Now backed by ChannelStore instead of a single Pipeline.          //
	// ------------------------------------------------------------------ //
	apiServer := api.NewServer(channelStore, database)

	// ------------------------------------------------------------------ //
	// 5. Create the MCP server (Model Context Protocol / JSON-RPC 2.0).   //
	//    ZKA: This server reads only scalar DB rows — no PII exposure.     //
	// ------------------------------------------------------------------ //
	mcpServer := mcp.NewMCPServer(database)

	// ------------------------------------------------------------------ //
	// 6. Resolve listen addresses from environment (with safe defaults).   //
	// ------------------------------------------------------------------ //
	apiAddr := envOrDefault("BURNOUT_API_ADDR", ":8080")
	mcpAddr := envOrDefault("BURNOUT_MCP_ADDR", ":8081")

	// ------------------------------------------------------------------ //
	// 7. Build the MCP HTTP server with timeouts.                          //
	//    The API server constructs its own *http.Server internally in      //
	//    api.Server.Start(); only the MCP server is managed directly here.  //
	// ------------------------------------------------------------------ //
	httpMCP := &http.Server{
		Addr:         mcpAddr,
		Handler:      mcpServer,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// ------------------------------------------------------------------ //
	// 8. Set up OS signal handler for graceful shutdown.                   //
	//    Catches SIGINT (Ctrl-C) and SIGTERM (systemd / container stop).   //
	// ------------------------------------------------------------------ //
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// ------------------------------------------------------------------ //
	// 9. Start both servers in goroutines so they run concurrently.       //
	// ------------------------------------------------------------------ //
	errCh := make(chan error, 2) // buffer 2: one slot per server

	go func() {
		log.Printf("main: API server starting on %s", apiAddr)
		// api.Server.Start builds its own mux and calls ListenAndServe.
		// We capture the error in errCh for main to handle.
		if err := apiServer.Start(apiAddr); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		} else {
			errCh <- nil
		}
	}()

	go func() {
		log.Printf("main: MCP server starting on %s", mcpAddr)
		if err := httpMCP.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		} else {
			errCh <- nil
		}
	}()

	// ------------------------------------------------------------------ //
	// 10. Start the background evaluation ticker.                          //
	//     Runs in its own goroutine; stops when shutdownCtx is cancelled.  //
	//     Interval: BURNOUT_EVAL_INTERVAL env var (default 60s).           //
	//     Target:   BURNOUT_DENO_URL env var (default http://localhost:8080)//
	// ------------------------------------------------------------------ //
	evalInterval := 60 * time.Second
	if raw := os.Getenv("BURNOUT_EVAL_INTERVAL"); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil {
			evalInterval = parsed
		} else {
			log.Printf("main: invalid BURNOUT_EVAL_INTERVAL %q, using default 60s: %v", raw, err)
		}
	}
	denoBaseURL := envOrDefault("BURNOUT_DENO_URL", "http://localhost:8080")
	denoMetricsURL := denoBaseURL + "/metrics"

	// tickerCtx is cancelled on shutdown, which cleanly stops the ticker goroutine.
	tickerCtx, tickerCancel := context.WithCancel(context.Background())
	defer tickerCancel()
	StartEvaluationTicker(tickerCtx, evalInterval, denoMetricsURL, channelStore, database)
	log.Printf("main: evaluation ticker started (interval=%s → %s)", evalInterval, denoMetricsURL)

	// ------------------------------------------------------------------ //
	// 11. Block until a signal or a fatal server error arrives.           //
	// ------------------------------------------------------------------ //
	select {
	case sig := <-sigCh:
		log.Printf("main: received signal %v — initiating graceful shutdown", sig)

	case err := <-errCh:
		if err != nil {
			log.Fatalf("main: server error: %v", err)
		}
	}

	// ------------------------------------------------------------------ //
	// 11. Graceful shutdown — give in-flight requests up to 15 seconds.  //
	// ------------------------------------------------------------------ //
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Shut down the MCP HTTP server.
	if err := httpMCP.Shutdown(shutdownCtx); err != nil {
		log.Printf("main: MCP server shutdown error: %v", err)
	} else {
		log.Println("main: MCP server stopped cleanly")
	}

	// The API server runs via api.Server.Start which manages its own *http.Server
	// internally. In production: refactor api.Server to expose the *http.Server
	// so it can be gracefully shut down here alongside httpMCP.
	// For now we rely on the 15-second context timeout draining in-flight requests.

	log.Println("main: BurnoutRadar MCP Daemon shutdown complete")
}

// envOrDefault returns the value of the named environment variable, or
// defaultVal if the variable is unset or empty.
func envOrDefault(name, defaultVal string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return defaultVal
}

// channelEntry holds a parsed channel ID and display name.
type channelEntry struct {
	ID   string
	Name string
}

// parseMonitoredChannels parses the MONITORED_CHANNELS environment variable.
//
// Expected format (comma-separated, colon-delimited ID:name pairs):
//
//	MONITORED_CHANNELS="C123ABC:backend-team,C456DEF:general,C789GHI"
//
// If a name is omitted (no colon), the channel ID is used as the display name.
// Blank entries are skipped silently.
func parseMonitoredChannels(raw string) []channelEntry {
	if strings.TrimSpace(raw) == "" {
		return nil
	}

	parts := strings.Split(raw, ",")
	entries := make([]channelEntry, 0, len(parts))

	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}

		idx := strings.IndexByte(p, ':')
		if idx == -1 {
			// No colon — use the whole token as both ID and name.
			entries = append(entries, channelEntry{ID: p, Name: p})
		} else {
			id := strings.TrimSpace(p[:idx])
			name := strings.TrimSpace(p[idx+1:])
			if name == "" {
				name = id // fall back to ID if name part is empty
			}
			entries = append(entries, channelEntry{ID: id, Name: name})
		}
	}

	return entries
}

