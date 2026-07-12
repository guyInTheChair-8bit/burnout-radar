// Package main — worker.go
//
// StartEvaluationTicker launches a background goroutine that periodically
// flushes every registered channel's ZKA pipeline, computes burnout scalars,
// evaluates the three threshold rules, and — for each channel that breaches a
// rule — fires an independent HTTP POST to the Deno Slack Agent's /metrics
// endpoint.
//
// Multi-channel pipeline:
//
//	time.Ticker
//	  └─ store.FlushAll()           — flush every channel, get anonymous []int
//	       └─ per channel:
//	            ├─ CalculateGini / ParetoTop20 / ZScore
//	            ├─ evaluateThresholds()    — which rule fired? (or none)
//	            ├─ store.UpdateSnapshot()  — persist scalars back into state
//	            ├─ db.InsertMetrics()      — write scalars to SQLite
//	            └─ postMetricsToDenoServer() — only if anomaly detected
//
// Each channel is evaluated and dispatched independently — a healthy channel
// is never included in a POST triggered by an anomalous sibling.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"burnoutradar-mcp/analytics"
	"burnoutradar-mcp/db"
	"burnoutradar-mcp/store"
)

// ---------------------------------------------------------------------------
// Payload schema (unchanged — matches Deno /metrics JSON contract)
// ---------------------------------------------------------------------------

// ChannelMetricsPayload matches the exact JSON structure expected by the
// Deno server's POST /metrics endpoint.
type ChannelMetricsPayload struct {
	ChannelID            string  `json:"channel_id"`
	ChannelName          string  `json:"channel_name"`
	Date                 string  `json:"date"`
	ZScore               float64 `json:"z_score"`
	GiniCoeff            float64 `json:"gini_coeff"`
	ParetoTop20Share     float64 `json:"pareto_top_20_share"`
	SentReceivedRatio    float64 `json:"sent_received_ratio"`
	DMSharePct           float64 `json:"dm_share_pct"`
	AvgWordCount         float64 `json:"avg_word_count"`
	AvgWordCountBaseline float64 `json:"avg_word_count_baseline"`
}

// metricsEnvelope wraps the payload under the "metrics" key as required
// by the Deno server's JSON schema.
type metricsEnvelope struct {
	Metrics ChannelMetricsPayload `json:"metrics"`
}

// ---------------------------------------------------------------------------
// Threshold evaluation — the 3 burnout rules
// ---------------------------------------------------------------------------

// riskProfile enumerates the four possible outcomes of threshold evaluation.
type riskProfile string

const (
	riskNone            riskProfile = "none"
	riskKeyPersonDep    riskProfile = "key_person_dependency"
	riskSystemicCrunch  riskProfile = "systemic_crunch_time"
	riskSilentIsolation riskProfile = "silent_isolation"
)

// evaluateThresholds applies the three burnout indicator rules to the scalar
// snapshot for a single channel and returns the first matching risk profile.
//
// Rules (in priority order — first match wins):
//
//  1. Key Person Dependency : Z > 2.0  AND Gini > 0.7 AND Pareto > 85%
//  2. Systemic Crunch Time  : Z > 2.0  AND Gini < 0.4 AND Sent/Rcv < 0.7
//  3. Silent Isolation      : Z < 1.0  AND DM% > 70   AND word-count drop > 30%
func evaluateThresholds(
	gini, pareto, zScore float64,
	sentRecv, dmShare float64,
	avgWordCount, avgWordCountBaseline float64,
) riskProfile {
	// Rule 1 — Key Person Dependency
	if zScore > 2.0 && gini > 0.7 && pareto > 85.0 {
		return riskKeyPersonDep
	}

	// Rule 2 — Systemic Crunch Time
	if zScore > 2.0 && gini < 0.4 && sentRecv < 0.7 {
		return riskSystemicCrunch
	}

	// Rule 3 — Silent Isolation
	// Word-count decline = (baseline - current) / baseline
	wordCountDrop := 0.0
	if avgWordCountBaseline > 0 {
		wordCountDrop = (avgWordCountBaseline - avgWordCount) / avgWordCountBaseline
	}
	if zScore < 1.0 && dmShare > 70.0 && wordCountDrop > 0.30 {
		return riskSilentIsolation
	}

	return riskNone
}

// ---------------------------------------------------------------------------
// HTTP dispatch — fires only for channels that breach a threshold
// ---------------------------------------------------------------------------

// postMetricsToDenoServer serialises the payload for a SINGLE channel and
// fires a POST to the Deno Slack Agent's /metrics endpoint.
//
// Errors are logged and the function returns — the caller's ticker loop
// continues processing remaining channels unaffected.
func postMetricsToDenoServer(payload ChannelMetricsPayload, denoURL string, client *http.Client) {
	envelope := metricsEnvelope{Metrics: payload}

	body, err := json.Marshal(envelope)
	if err != nil {
		log.Printf("[worker] ERROR: json.Marshal failed for channel #%s: %v",
			payload.ChannelName, err)
		return
	}

	token := os.Getenv("SLACK_BOT_TOKEN")
	if token == "" {
		log.Println("[worker] WARNING: SLACK_BOT_TOKEN not set; omitting Authorization header")
	}

	// Per-request timeout so a hung Deno server cannot stall the goroutine.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, denoURL, bytes.NewReader(body))
	if err != nil {
		log.Printf("[worker] ERROR: building request to %s: %v", denoURL, err)
		return
	}

	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	}

	log.Printf("[worker] → POST %s  channel=#%s  risk=%s  z=%.2f gini=%.2f pareto=%.1f%%",
		denoURL, payload.ChannelName, payload.ChannelID,
		payload.ZScore, payload.GiniCoeff, payload.ParetoTop20Share)

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[worker] ERROR: POST %s unreachable: %v", denoURL, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("[worker] WARNING: Deno returned HTTP %d for channel #%s",
			resp.StatusCode, payload.ChannelName)
		return
	}

	log.Printf("[worker] ✓ Deno accepted alert for channel #%s (HTTP %d)",
		payload.ChannelName, resp.StatusCode)
}

// ---------------------------------------------------------------------------
// Per-channel evaluation — called once per channel on each tick
// ---------------------------------------------------------------------------

// evaluateChannel flushes one channel's pipeline, computes burnout scalars,
// updates the store snapshot, writes to SQLite, and dispatches to Deno if an
// anomaly is detected.
//
// This function is called sequentially for each channel on the ticker goroutine.
// For very large numbers of channels, each call could be launched in its own
// goroutine — the store's locking design supports that without modification.
func evaluateChannel(
	result store.FlushResult,
	denoURL string,
	client *http.Client,
	cs *store.ChannelStore,
	database *db.DB,
) {
	today := time.Now().UTC().Format("2006-01-02")

	// ── Compute ZKA-safe scalars from the anonymous []int slice ──────────
	gini := analytics.CalculateGini(result.Counts)
	pareto := analytics.CalculateParetoTop20(result.Counts)
	zScore := analytics.CalculateZScore(
		result.Stats.TotalMessages,
		result.HistoricalMean,
		result.HistoricalStdDev,
	)

	// ZKA: counts slice has served its purpose — nil it.
	result.Counts = nil

	log.Printf("[worker] channel=#%s  msgs=%d  z=%.2f  gini=%.2f  pareto=%.1f%%  dm=%.1f%%",
		result.ChannelName, result.Stats.TotalMessages,
		zScore, gini, pareto, result.Stats.DMSharePct)

	// ── Persist scalars to SQLite (ZKA: no user data reaches DB) ─────────
	if err := database.InsertMetrics(
		today, result.ChannelID,
		gini, pareto, zScore,
		result.Stats.DMSharePct, result.Stats.AvgWordCount,
	); err != nil {
		log.Printf("[worker] ERROR: InsertMetrics for channel %s: %v",
			result.ChannelID, err)
		// Non-fatal — continue to threshold evaluation.
	}

	// ── Update the channel's scalar snapshot in the store ─────────────────
	cs.UpdateSnapshot(result.ChannelID, store.ChannelMetricsSnapshot{
		GiniCoeff:            gini,
		ParetoTop20Share:     pareto,
		ZScore:               zScore,
		SentReceivedRatio:    result.Stats.SentReceivedRatio,
		DMSharePct:           result.Stats.DMSharePct,
		AvgWordCount:         result.Stats.AvgWordCount,
		AvgWordCountBaseline: result.AvgWordCountBaseline,
		TotalMessages:        result.Stats.TotalMessages,
		ComputedAt:           time.Now().UTC(),
	})

	// ── Evaluate the three burnout threshold rules independently ──────────
	risk := evaluateThresholds(
		gini, pareto, zScore,
		result.Stats.SentReceivedRatio,
		result.Stats.DMSharePct,
		result.Stats.AvgWordCount,
		result.AvgWordCountBaseline,
	)

	if risk == riskNone {
		log.Printf("[worker] channel=#%s — no anomaly detected", result.ChannelName)
		return
	}

	// ── Anomaly detected — build payload and POST to Deno ─────────────────
	log.Printf("[worker] ANOMALY channel=#%s risk=%s", result.ChannelName, risk)

	payload := ChannelMetricsPayload{
		ChannelID:            result.ChannelID,
		ChannelName:          result.ChannelName,
		Date:                 today,
		ZScore:               zScore,
		GiniCoeff:            gini,
		ParetoTop20Share:     pareto,
		SentReceivedRatio:    result.Stats.SentReceivedRatio,
		DMSharePct:           result.Stats.DMSharePct,
		AvgWordCount:         result.Stats.AvgWordCount,
		AvgWordCountBaseline: result.AvgWordCountBaseline,
	}

	postMetricsToDenoServer(payload, denoURL, client)
}

// ---------------------------------------------------------------------------
// Ticker launcher
// ---------------------------------------------------------------------------

// StartEvaluationTicker starts the multi-channel background evaluation loop.
//
// On every tick it:
//  1. Calls store.FlushAll() to collect anonymous flush data from all channels.
//  2. Evaluates each channel independently via evaluateChannel().
//  3. Only fires a POST to Deno for channels that breach a burnout threshold.
//
// The ticker goroutine stops cleanly when ctx is cancelled (SIGTERM/SIGINT).
// The function itself returns immediately — it never blocks the caller.
//
// Parameters:
//   - ctx      — governs lifetime; cancel to stop the ticker.
//   - interval — evaluation cadence (60 s default; BURNOUT_EVAL_INTERVAL env).
//   - denoURL  — full URL of the Deno /metrics endpoint.
//   - cs       — the multi-channel store; FlushAll() is called on every tick.
//   - database — SQLite handle for persisting scalar metrics.
func StartEvaluationTicker(
	ctx context.Context,
	interval time.Duration,
	denoURL string,
	cs *store.ChannelStore,
	database *db.DB,
) {
	// Shared HTTP client — connection pool is reused across all channels and ticks.
	client := &http.Client{Timeout: 15 * time.Second}
	ticker := time.NewTicker(interval)

	go func() {
		defer ticker.Stop()
		log.Printf("[worker] multi-channel ticker started (interval=%s, target=%s, channels=%d)",
			interval, denoURL, cs.Len())

		for {
			select {
			case <-ctx.Done():
				log.Println("[worker] ticker stopped (context cancelled)")
				return

			case t := <-ticker.C:
				log.Printf("[worker] tick at %s — flushing %d channel(s)",
					t.UTC().Format(time.RFC3339), cs.Len())

				// Flush ALL channels and get per-channel anonymous results.
				// Channels with zero activity this window are skipped automatically.
				results := cs.FlushAll()

				if len(results) == 0 {
					log.Println("[worker] no channel activity this window — skipping evaluation")
					continue
				}

				// Evaluate each channel independently.
				// A healthy channel never triggers a dispatch, even if a sibling anomalies.
				for _, result := range results {
					evaluateChannel(result, denoURL, client, cs, database)
				}
			}
		}
	}()
}
