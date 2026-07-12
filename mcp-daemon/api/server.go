// Package api provides the HTTP server that receives Slack webhook events and
// triggers analytics pipeline flushes for BurnoutRadar MCP Daemon.
//
// ZKA NOTE: This server is the *entry point* for raw PII. It is responsible for
// immediately routing payloads into the ZKA pipeline (analytics.Pipeline) where
// the PII is destroyed. The server itself never logs or stores user identifiers.
package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"burnoutradar-mcp/analytics"
	"burnoutradar-mcp/db"
	"burnoutradar-mcp/store"
)

// Historical baseline constants used for z-score calculation.
// In production these should come from a rolling DB query; hardcoded for now.
const (
	historicalMean   = 100.0 // expected messages per channel per window
	historicalStdDev = 20.0  // expected standard deviation
)

// slackChallengeRequest is used to handle Slack's URL verification handshake.
// See: https://api.slack.com/events/url_verification
type slackChallengeRequest struct {
	Token     string `json:"token"`
	Challenge string `json:"challenge"`
	Type      string `json:"type"`
}

// Server holds a reference to the multi-channel ChannelStore and the DB,
// providing HTTP handler methods that wire them together.
type Server struct {
	store    *store.ChannelStore
	database *db.DB
}

// NewServer constructs a Server with the given channel store and database.
func NewServer(cs *store.ChannelStore, database *db.DB) *Server {
	return &Server{
		store:    cs,
		database: database,
	}
}

// handleSlackWebhook processes incoming Slack event webhook payloads (POST).
//
// ZKA flow:
//  1. Decode JSON into analytics.SlackWebhookPayload (transient struct).
//  2. Handle Slack URL-verification challenge (no PII involved).
//  3. Route the payload to the correct channel's pipeline via store.Process().
//     If the channel_id is not in MONITORED_CHANNELS, the event is dropped.
//  4. Respond 200 OK. The raw payload never touches disk or logs.
func (s *Server) handleSlackWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		http.Error(w, "bad request: invalid JSON", http.StatusBadRequest)
		return
	}

	// --- Slack URL verification challenge ---
	if typeRaw, ok := raw["type"]; ok {
		var msgType string
		if err := json.Unmarshal(typeRaw, &msgType); err == nil && msgType == "url_verification" {
			var challengeReq slackChallengeRequest
			tmp, _ := json.Marshal(raw)
			if err := json.Unmarshal(tmp, &challengeReq); err != nil {
				http.Error(w, "bad challenge request", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"challenge": challengeReq.Challenge})
			return
		}
	}

	// --- Normal event processing ---
	// Reconstruct the transient payload struct from the raw JSON map.
	// ZKA: UserID and Text are zeroed immediately inside pipeline.Process().
	var payload analytics.SlackWebhookPayload

	if v, ok := raw["channel_id"]; ok {
		json.Unmarshal(v, &payload.ChannelID)
	}
	if v, ok := raw["user_id"]; ok {
		json.Unmarshal(v, &payload.UserID) // ZKA: zeroed in pipeline.Process()
	}
	if v, ok := raw["text"]; ok {
		json.Unmarshal(v, &payload.Text) // ZKA: zeroed in pipeline.Process()
	}
	if v, ok := raw["is_dm"]; ok {
		json.Unmarshal(v, &payload.IsDM)
	}
	if v, ok := raw["timestamp"]; ok {
		json.Unmarshal(v, &payload.Timestamp)
	}

	// Route to the correct channel's pipeline.
	// store.Process() returns false if channel_id is not in MONITORED_CHANNELS.
	if !s.store.Process(payload.ChannelID, payload) {
		log.Printf("api: webhook for unmonitored channel %q — dropped", payload.ChannelID)
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, `{"status":"ok"}`)
}

// handleFlush triggers a ZKA-safe pipeline flush for a specific channel and
// persists the resulting scalar metrics to SQLite.
//
// Query params:
//   - channel_id (required): the Slack channel ID to flush.
//
// In normal operation the background worker handles flushing automatically.
// This endpoint is for manual operator triggers and testing.
func (s *Server) handleFlush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	channelID := r.URL.Query().Get("channel_id")
	if channelID == "" {
		http.Error(w, "missing channel_id query parameter", http.StatusBadRequest)
		return
	}

	// Flush the specific channel via the store.
	// ZKA: FlushChannel destroys the ephemeral map; only []int counts are returned.
	result, ok := s.store.FlushChannel(channelID)
	if !ok {
		http.Error(w, "channel not found or no data to flush", http.StatusNotFound)
		return
	}

	// Compute statistical scalars from the anonymous counts slice.
	gini := analytics.CalculateGini(result.Counts)
	pareto := analytics.CalculateParetoTop20(result.Counts)
	zScore := analytics.CalculateZScore(
		result.Stats.TotalMessages,
		result.HistoricalMean,
		result.HistoricalStdDev,
	)

	// ZKA: counts no longer needed — release for GC.
	result.Counts = nil

	date := time.Now().UTC().Format("2006-01-02")
	if err := s.database.InsertMetrics(
		date, channelID,
		gini, pareto, zScore,
		result.Stats.DMSharePct, result.Stats.AvgWordCount,
	); err != nil {
		log.Printf("api: flush: InsertMetrics error: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":         "flushed",
		"date":           date,
		"channel_id":     channelID,
		"channel_name":   result.ChannelName,
		"gini_coeff":     gini,
		"pareto_share":   pareto,
		"z_score":        zScore,
		"dm_share_pct":   result.Stats.DMSharePct,
		"avg_word_count": result.Stats.AvgWordCount,
		"total_messages": result.Stats.TotalMessages,
	})
}

// handleGetMetrics handles GET /api/metrics?channel_id=XXX
//
// Used by the Deno Slack Agent's /burnout slash command handler to pull
// the latest computed scalars for a specific channel on demand.
//
// Response:
//   - 200 + JSON SnapshotResponse  — channel found with computed metrics
//   - 404 + JSON error             — channel unknown or no metrics yet
//   - 400 + JSON error             — missing channel_id parameter
//   - 405                          — non-GET method
//
// ZKA: Only scalar metrics are returned — no user identity in any field.
func (s *Server) handleGetMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	channelID := r.URL.Query().Get("channel_id")
	if channelID == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"missing channel_id query parameter"}`)
		return
	}

	// Thread-safe read via two-level locking in GetSnapshot.
	snap, ok := s.store.GetSnapshot(channelID)
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w,
			`{"error":"channel %q not found or metrics not yet computed — wait for the next evaluation tick"}`,
			channelID,
		)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store") // always return fresh data
	if err := json.NewEncoder(w).Encode(snap); err != nil {
		log.Printf("api: handleGetMetrics: encode error: %v", err)
	}
}

// Start registers HTTP routes and begins serving on the given address.
// It blocks until the server exits.
func (s *Server) Start(addr string) error {
	mux := http.NewServeMux()

	// Webhook endpoint — receives raw Slack events.
	mux.HandleFunc("/webhook/slack", s.handleSlackWebhook)

	// Flush endpoint — manual per-channel pipeline flush and DB write.
	// In production, protect this with an HMAC-signed token middleware.
	mux.HandleFunc("/flush", s.handleFlush)

	// Metrics read endpoint — used by Deno /burnout slash command handler.
	// GET /api/metrics?channel_id=XXX
	mux.HandleFunc("/api/metrics", s.handleGetMetrics)

	// Simple health check — useful for load balancers and uptime monitors.
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"status":"healthy"}`)
	})

	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	log.Printf("api: listening on %s", addr)
	return srv.ListenAndServe()
}
