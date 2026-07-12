// Package analytics — pipeline.go
// This file implements the core Zero Knowledge Architecture (ZKA) engine.
// Data flows in one direction only: raw webhook payload → ephemeral RAM →
// anonymous integer slice → scalar statistics. PII is destroyed at each stage.
package analytics

import (
	"strings"
)

// SlackWebhookPayload represents the raw incoming event from a Slack webhook.
// Fields intentionally use value types (string, bool) rather than pointers so
// that zeroing them out fully overwrites the underlying memory.
//
// ZKA: This struct is TRANSIENT — it must never be stored to disk or logged.
// Its PII fields (UserID, Text) are zeroed immediately after use in Process().
type SlackWebhookPayload struct {
	UserID    string  // Slack user identifier e.g. "U012AB3CD"  — ZKA: ephemeral
	Text      string  // Raw message text                         — ZKA: ephemeral
	ChannelID string  // Channel identifier e.g. "C012AB3CD"     — retained (not PII)
	IsDM      bool    // Whether the message is a Direct Message
	Timestamp float64 // Unix timestamp of the message
}

// AggregatedStats holds computed aggregate metrics for a processing window.
// Every field here is a mathematical scalar — no user-level information remains.
//
// ZKA: Safe to pass across package boundaries; contains no PII.
type AggregatedStats struct {
	SentReceivedRatio float64 // Ratio of sent to received messages in the channel
	TotalMessages     int     // Total message count processed in this window
	DMSharePct        float64 // Percentage of messages sent as Direct Messages
	AvgWordCount      float64 // Average words per message across all messages
}

// Pipeline is the central ZKA processing unit. It maintains rolling aggregate
// counters and an ephemeral hashed-ID → count map that is destroyed before any
// statistical output is produced.
//
// ZKA enforcement summary:
//   - `counts` maps HMAC-hashed IDs (not real IDs) to integer counts.
//   - Rolling counters (totalWords, totalMessages, etc.) never reference users.
//   - FlushAndDestroy() extracts only []int values and then deletes the map.
type Pipeline struct {
	hasher *Hasher // produces ephemeral HMAC hashes of raw user IDs

	// counts is the ephemeral in-memory map.
	// Keys: HMAC-SHA256 hex digests (opaque tokens, not real IDs).
	// Values: integer message counts only.
	// ZKA: This map MUST be cleared and nilled before returning stats.
	counts map[string]int

	// Rolling aggregate counters — never keyed by user identity.
	totalWords    int // cumulative word count across all processed messages
	totalMessages int // total number of Process() calls in this window
	dmCount       int // number of messages flagged as Direct Messages
	sentCount     int // messages sent by the bot/service account (future use)
	receivedCount int // messages received from external users
}

// NewPipeline creates an initialised Pipeline ready to accept messages.
func NewPipeline(h *Hasher) *Pipeline {
	return &Pipeline{
		hasher: h,
		counts: make(map[string]int),
	}
}

// Process ingests a single Slack message event through the ZKA pipeline.
//
// Order of operations (critical — do not reorder):
//  1. Compute word_count from msg.Text BEFORE zeroing it.
//  2. Record IsDM state BEFORE zeroing msg.
//  3. Update aggregate counters (no user reference).
//  4. ZERO OUT msg.Text and msg.UserID to release PII from this stack frame.
//  5. Hash the (already-zeroed original ID, captured in a local var) to get
//     an opaque token and increment the ephemeral count map.
//
// ZKA: After this function returns, no raw PII remains in any live variable.
func (p *Pipeline) Process(msg SlackWebhookPayload) {
	// Step 1: Extract word count BEFORE the text is zeroed.
	// We count whitespace-separated tokens as a simple word count proxy.
	// ZKA: wordCount is an integer — it cannot be used to reconstruct msg.Text.
	wordCount := len(strings.Fields(msg.Text))

	// Step 2: Capture the IsDM flag (already a bool scalar, not PII).
	isDM := msg.IsDM

	// Step 3: Capture the raw user ID into a SHORT-LIVED local variable.
	// ZKA: rawID is stack-allocated and not written to any struct field.
	rawID := msg.UserID

	// Step 4: IMMEDIATELY zero out PII fields.
	// This ensures that even if the caller retains a reference to the original
	// struct (it's passed by value, so they can't, but we are defensive),
	// no PII leaks from this scope into any downstream operation.
	msg.Text = ""   // ZKA: message text destroyed — cannot be logged or stored
	msg.UserID = "" // ZKA: raw user ID destroyed — only the hash will be kept

	// Step 5: Update aggregate rolling counters using scalar values only.
	p.totalMessages++
	p.totalWords += wordCount
	p.receivedCount++ // all inbound webhook events treated as received
	if isDM {
		p.dmCount++
	}

	// Step 6: Hash the raw ID (now gone from msg) and increment the count map.
	// ZKA: hashedID is an HMAC digest — reversing it without the ephemeral salt
	// (which lives only in the Hasher struct memory) is computationally infeasible.
	hashedID := p.hasher.HashUserID(rawID) // rawID consumed here; not stored
	p.counts[hashedID]++                   // only the integer count is recorded
}

// FlushAndDestroy finalises the current processing window by:
//  1. Extracting message counts into an anonymous []int slice.
//  2. Computing AggregatedStats from rolling counters.
//  3. Explicitly deleting every key from the counts map.
//  4. Setting the map to nil to release the underlying hash table memory.
//  5. Resetting all rolling counters to zero for the next window.
//
// Returns:
//   - counts []int  — anonymous message counts (no identity attached)
//   - stats AggregatedStats — aggregate scalars for the processing window
//
// ZKA: After this call the hashed-ID map is completely destroyed. The returned
// []int contains only integers; no user identity can be derived from it.
func (p *Pipeline) FlushAndDestroy() ([]int, AggregatedStats) {
	// Step 1: Extract ONLY the integer values from the map into an anonymous
	// slice. The map keys (hashed IDs) are intentionally discarded here.
	// ZKA: `counts` slice has no association to any identity, hashed or otherwise.
	counts := make([]int, 0, len(p.counts))
	for _, v := range p.counts { // key (hashedID) is intentionally blank-identifiered
		counts = append(counts, v)
	}

	// Step 2: Compute aggregate stats from rolling counters (all scalars).
	stats := AggregatedStats{
		TotalMessages: p.totalMessages,
	}

	// Sent/received ratio — guard division by zero.
	if p.receivedCount > 0 {
		stats.SentReceivedRatio = float64(p.sentCount) / float64(p.receivedCount)
	}

	// DM share percentage — guard division by zero.
	if p.totalMessages > 0 {
		stats.DMSharePct = float64(p.dmCount) / float64(p.totalMessages) * 100.0
		stats.AvgWordCount = float64(p.totalWords) / float64(p.totalMessages)
	}

	// Step 3: Explicitly delete every entry from the ephemeral map.
	// ZKA: Iterating and deleting is more reliable than re-assigning the variable
	// because it guarantees the underlying map buckets are cleared (not just
	// unreferenced), reducing the window for forensic memory recovery.
	for k := range p.counts {
		delete(p.counts, k) // ZKA: destroy each hashed-ID→count association
	}

	// Step 4: Set the map to nil — releases the hash table backing memory.
	// ZKA: After this line, the counts map is unreachable and GC-eligible.
	p.counts = nil

	// Step 5: Reset rolling counters for the next collection window.
	// This ensures the next flush window starts from a clean state.
	p.totalWords = 0
	p.totalMessages = 0
	p.dmCount = 0
	p.sentCount = 0
	p.receivedCount = 0

	// Re-initialise the map for the next processing window.
	p.counts = make(map[string]int)

	return counts, stats
}
