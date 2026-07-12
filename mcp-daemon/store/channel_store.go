// Package store — channel_store.go
//
// ChannelStore is the thread-safe registry of all monitored Slack channels.
// It holds one analytics.Pipeline per channel so that the ZKA hashing,
// ephemeral maps, and FlushAndDestroy lifecycle are fully isolated per channel.
//
// Concurrency model:
//   - The outer map (channels) is protected by a top-level sync.RWMutex.
//     Multiple readers (e.g. worker iterating + webhook routing) may hold
//     concurrent read locks; writers (Register) take an exclusive lock.
//   - Each ChannelState owns a sync.Mutex that serialises access to its
//     Pipeline. This means concurrent webhooks for different channels never
//     block each other — only simultaneous writes to the *same* channel's
//     pipeline are serialised.
package store

import (
	"sync"
	"time"

	"burnoutradar-mcp/analytics"
)

// ---------------------------------------------------------------------------
// ChannelMetricsSnapshot — ZKA-safe scalar snapshot
// ---------------------------------------------------------------------------

// ChannelMetricsSnapshot holds the most recently computed scalar metrics for
// a single channel after a pipeline flush. Every field is a mathematical
// scalar — no user identity survives into this struct.
type ChannelMetricsSnapshot struct {
	GiniCoeff            float64
	ParetoTop20Share     float64
	ZScore               float64
	SentReceivedRatio    float64
	DMSharePct           float64
	AvgWordCount         float64
	AvgWordCountBaseline float64 // rolling baseline seeded at registration
	TotalMessages        int
	ComputedAt           time.Time
}

// ---------------------------------------------------------------------------
// ChannelState — per-channel live state
// ---------------------------------------------------------------------------

// ChannelState holds the live pipeline and last-computed metrics for one
// monitored Slack channel.
//
// mu serialises all reads and writes to Pipeline and Snapshot within this
// specific channel's state, independently of other channels.
type ChannelState struct {
	mu sync.Mutex // protects Pipeline and Snapshot below

	ChannelID   string
	ChannelName string

	// Pipeline is the per-channel ZKA engine. It holds an ephemeral
	// hashed-ID → count map that is destroyed on every FlushAndDestroy() call.
	Pipeline *analytics.Pipeline

	// Historical baseline parameters for z-score calculation.
	// In production these should come from a rolling DB query.
	HistoricalMean   float64
	HistoricalStdDev float64

	// Snapshot is updated by the worker on each successful flush.
	// It is the last known scalar state of this channel.
	Snapshot ChannelMetricsSnapshot
}

// ---------------------------------------------------------------------------
// ChannelStore — top-level concurrent registry
// ---------------------------------------------------------------------------

// ChannelStore is a concurrent-safe registry of all monitored Slack channels.
type ChannelStore struct {
	mu       sync.RWMutex
	channels map[string]*ChannelState

	// Default config for auto-registering new channels
	hasher          *analytics.Hasher
	defaultMean     float64
	defaultStdDev   float64
	defaultBaseline float64
}

// NewChannelStore creates an empty, ready-to-use ChannelStore.
func NewChannelStore(hasher *analytics.Hasher, defaultMean, defaultStdDev, defaultBaseline float64) *ChannelStore {
	return &ChannelStore{
		channels:        make(map[string]*ChannelState),
		hasher:          hasher,
		defaultMean:     defaultMean,
		defaultStdDev:   defaultStdDev,
		defaultBaseline: defaultBaseline,
	}
}

// autoRegister creates a new ChannelState safely.
func (cs *ChannelStore) autoRegister(channelID, channelName string) *ChannelState {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	if state, exists := cs.channels[channelID]; exists {
		// Update name if we learned a better one
		if state.ChannelName == state.ChannelID && channelName != channelID {
			state.ChannelName = channelName
		}
		return state
	}

	state := &ChannelState{
		ChannelID:        channelID,
		ChannelName:      channelName,
		Pipeline:         analytics.NewPipeline(cs.hasher),
		HistoricalMean:   cs.defaultMean,
		HistoricalStdDev: cs.defaultStdDev,
		Snapshot: ChannelMetricsSnapshot{
			AvgWordCountBaseline: cs.defaultBaseline,
		},
	}
	cs.channels[channelID] = state
	return state
}

// Register adds a new channel to the store manually.
// Safe to call at startup before any goroutines are running.
func (cs *ChannelStore) Register(channelID, channelName string) {
	cs.autoRegister(channelID, channelName)
}

// ---------------------------------------------------------------------------
// Process — route a webhook event to the correct channel pipeline
// ---------------------------------------------------------------------------

// Process routes an incoming Slack message event to the correct channel's
// ZKA pipeline. Auto-registers the channel if it's not yet monitored.
//
// Concurrency: takes a read lock on the outer map to locate the channel,
// then takes the channel's own mutex before calling Pipeline.Process().
func (cs *ChannelStore) Process(channelID string, msg analytics.SlackWebhookPayload) bool {
	cs.mu.RLock()
	state, ok := cs.channels[channelID]
	cs.mu.RUnlock()

	if !ok {
		// Auto-register using channelID as the fallback name
		state = cs.autoRegister(channelID, channelID)
	}

	// Serialise access to this channel's pipeline only.
	state.mu.Lock()
	state.Pipeline.Process(msg)
	state.mu.Unlock()

	return true
}

// ---------------------------------------------------------------------------
// FlushResult — data returned from a single channel's flush
// ---------------------------------------------------------------------------

// FlushResult carries the anonymous data extracted from one channel's pipeline
// during a FlushAll or FlushChannel call. All fields are ZKA-safe scalars.
type FlushResult struct {
	ChannelID            string
	ChannelName          string
	Counts               []int // anonymous message-count distribution
	Stats                analytics.AggregatedStats
	HistoricalMean       float64
	HistoricalStdDev     float64
	AvgWordCountBaseline float64
}

// ---------------------------------------------------------------------------
// FlushAll — iterate and flush every registered channel
// ---------------------------------------------------------------------------

// FlushAll atomically flushes every registered channel's pipeline and returns
// their anonymous data for threshold evaluation. Channels with zero messages
// since the last flush are skipped (no alert needed).
//
// Concurrency: collects channel IDs under a brief read lock, then flushes
// each channel under its own per-channel mutex. The outer map lock is NOT
// held during individual flushes, so new webhook events can be processed
// concurrently for channels not currently being flushed.
func (cs *ChannelStore) FlushAll() []FlushResult {
	// Step 1: Snapshot the channel IDs under a read lock (fast path).
	cs.mu.RLock()
	ids := make([]string, 0, len(cs.channels))
	for id := range cs.channels {
		ids = append(ids, id)
	}
	cs.mu.RUnlock()

	results := make([]FlushResult, 0, len(ids))

	for _, id := range ids {
		// Re-acquire read lock per channel to fetch the state pointer safely.
		cs.mu.RLock()
		state, ok := cs.channels[id]
		cs.mu.RUnlock()
		if !ok {
			continue // channel was removed between snapshot and here (unlikely)
		}

		// Flush this channel's pipeline under its own mutex.
		state.mu.Lock()
		counts, stats := state.Pipeline.FlushAndDestroy()
		baseline := state.Snapshot.AvgWordCountBaseline
		hMean := state.HistoricalMean
		hStdDev := state.HistoricalStdDev
		name := state.ChannelName
		state.mu.Unlock()

		// Skip channels with no activity this window.
		if len(counts) == 0 {
			continue
		}

		results = append(results, FlushResult{
			ChannelID:            id,
			ChannelName:          name,
			Counts:               counts,
			Stats:                stats,
			HistoricalMean:       hMean,
			HistoricalStdDev:     hStdDev,
			AvgWordCountBaseline: baseline,
		})
	}

	return results
}

// ---------------------------------------------------------------------------
// FlushChannel — flush a single named channel (used by the /flush endpoint)
// ---------------------------------------------------------------------------

// FlushChannel flushes one specific channel by ID. Returns (result, true) if
// the channel exists and had data; (zero, false) otherwise.
func (cs *ChannelStore) FlushChannel(channelID string) (FlushResult, bool) {
	cs.mu.RLock()
	state, ok := cs.channels[channelID]
	cs.mu.RUnlock()

	if !ok {
		return FlushResult{}, false
	}

	state.mu.Lock()
	counts, stats := state.Pipeline.FlushAndDestroy()
	baseline := state.Snapshot.AvgWordCountBaseline
	hMean := state.HistoricalMean
	hStdDev := state.HistoricalStdDev
	name := state.ChannelName
	state.mu.Unlock()

	if len(counts) == 0 {
		return FlushResult{}, false
	}

	return FlushResult{
		ChannelID:            channelID,
		ChannelName:          name,
		Counts:               counts,
		Stats:                stats,
		HistoricalMean:       hMean,
		HistoricalStdDev:     hStdDev,
		AvgWordCountBaseline: baseline,
	}, true
}

// ---------------------------------------------------------------------------
// UpdateSnapshot — persist computed scalars back into the channel's state
// ---------------------------------------------------------------------------

// UpdateSnapshot stores the latest computed scalar metrics for a channel after
// a flush cycle. Called by the worker once it has computed Gini/Pareto/ZScore.
func (cs *ChannelStore) UpdateSnapshot(channelID string, snap ChannelMetricsSnapshot) {
	cs.mu.RLock()
	state, ok := cs.channels[channelID]
	cs.mu.RUnlock()

	if !ok {
		return
	}

	state.mu.Lock()
	state.Snapshot = snap
	state.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Len — diagnostics
// ---------------------------------------------------------------------------

// Len returns the number of currently registered channels.
func (cs *ChannelStore) Len() int {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return len(cs.channels)
}

// ---------------------------------------------------------------------------
// GetSnapshot — read-only view for the /api/metrics endpoint
// ---------------------------------------------------------------------------

// SnapshotResponse is the JSON-serialisable read-only view of a channel's
// latest computed scalar metrics. All fields are ZKA-safe — no user identity.
// JSON field names intentionally match the BurnoutMetrics TypeScript interface
// in the Deno Slack Agent so the Deno side can consume this directly.
type SnapshotResponse struct {
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
	TotalMessages        int     `json:"total_messages"`
}

// GetSnapshot returns a safe read-only scalar snapshot for the given channel.
// If the channel is not registered, it auto-registers it.
// If no metrics have been computed yet, it returns a synthetic "healthy" baseline.
func (cs *ChannelStore) GetSnapshot(channelID, channelName string) (*SnapshotResponse, bool) {
	cs.mu.RLock()
	state, ok := cs.channels[channelID]
	cs.mu.RUnlock()

	if !ok {
		state = cs.autoRegister(channelID, channelName)
	}

	state.mu.Lock()
	snap := state.Snapshot  // copy by value — safe to use after unlock
	name := state.ChannelName
	state.mu.Unlock()

	// If ComputedAt is zero the worker hasn't flushed this channel yet.
	// Return a healthy synthetic baseline so slash commands work immediately.
	if snap.ComputedAt.IsZero() {
		return &SnapshotResponse{
			ChannelID:            channelID,
			ChannelName:          name,
			Date:                 time.Now().UTC().Format("2006-01-02"),
			ZScore:               0.0,
			GiniCoeff:            0.0,
			ParetoTop20Share:     0.0,
			SentReceivedRatio:    1.0,
			DMSharePct:           0.0,
			AvgWordCount:         cs.defaultBaseline,
			AvgWordCountBaseline: cs.defaultBaseline,
			TotalMessages:        0,
		}, true
	}

	return &SnapshotResponse{
		ChannelID:            channelID,
		ChannelName:          name,
		Date:                 snap.ComputedAt.UTC().Format("2006-01-02"),
		ZScore:               snap.ZScore,
		GiniCoeff:            snap.GiniCoeff,
		ParetoTop20Share:     snap.ParetoTop20Share,
		SentReceivedRatio:    snap.SentReceivedRatio,
		DMSharePct:           snap.DMSharePct,
		AvgWordCount:         snap.AvgWordCount,
		AvgWordCountBaseline: snap.AvgWordCountBaseline,
		TotalMessages:        snap.TotalMessages,
	}, true
}

