# BurnoutRadar: Project Execution Tasks & Architecture Spec

You are tasked with generating the complete codebase for **BurnoutRadar**, an ambient, privacy-first Slack Agent that detects team burnout. The system consists of a Slack Agent (TypeScript/Deno) and an MCP Backend Daemon (Go). 

You must execute the following tasks in parallel, adhering strictly to the architecture, math, and UI constraints defined below.

## CORE ARCHITECTURE RULE: TRUE ZERO-KNOWLEDGE
Privacy is the primary technical constraint. You must implement a True Zero-Knowledge Architecture (ZKA):
1. **No Raw PII Storage:** Raw `user_id` and message `text` must NEVER touch a disk, database, or stdout.
2. **Ephemeral In-Memory Hashing:** Incoming `user_id`s must be hashed using `crypto/hmac` with a randomly generated daily salt. These hashes live exclusively in a temporary RAM map (`map[string]int`).
3. **Identity Destruction:** Before statistical calculations, the backend must extract ONLY the integer message counts into a slice (`[]int`). The `map[string]int` containing the hashed IDs must be explicitly destroyed/cleared from memory.
4. **Scalar Persistence:** The database (SQLite) may ONLY store the final mathematical scalars per channel (e.g., Gini coefficient, Pareto share).

## THE BURNOUT INDICATORS (THE 3 COMBOS)
The system uses mathematical thresholds to identify 3 specific behavioral burnout profiles. The Slack Agent must use the MCP tool to fetch the day's scalars, compare them against these triggers, and prompt Claude 3.5 Sonnet to generate the response.

1. **Key Person Dependency Risk** (Bottlenecking)
   - *Math Triggers:* Z-Score > 2.0 AND Gini Coefficient > 0.7 AND Pareto (Top 20% share) > 85%
   - *Meaning:* A small fraction of the team is taking on almost all after-hours work.
2. **Systemic Crunch Time Risk** (Team Overwhelm)
   - *Math Triggers:* Z-Score > 2.0 AND Gini Coefficient < 0.4 AND Sent/Received Ratio < 0.7
   - *Meaning:* The whole team is drowning in work and reacting to inbound requests rather than proactively collaborating.
3. **Silent Isolation Risk** (Emotional Depletion)
   - *Math Triggers:* Z-Score < 1.0 AND Public-to-DM Shift is HIGH (>70% DMs) AND Avg Word Count drops by >30% from baseline.
   - *Meaning:* Working normal hours, but pulling away from public collaboration due to context-switching fatigue.

---

## TASK 1: Generate the Go MCP Backend Daemon (`/mcp-daemon`)
Initialize a Go module (`go mod init burnoutradar-mcp`). Generate the following files:

- **`/analytics/hasher.go`**: 
  - Implement a struct that generates a secure, random daily salt on instantiation.
  - Write a method `HashUserID(rawID string) string` using HMAC-SHA256.
- **`/analytics/pipeline.go` (The ZKA Engine)**:
  - Implement the `Process(msg SlackWebhookPayload)` function. 
  - It must calculate `word_count` and `is_dm` first, add those to a rolling total, and then IMMEDIATELY discard the text.
  - It uses the hasher to increment the user's message count in an active `map[string]int`.
  - Implement `FlushAndDestroy() ([]int, AggregatedStats)`: Extracts values into an integer slice, calculates the Sent/Received ratio for the channel, explicitly clears the map from memory, and returns the anonymous integer slice.
- **`/analytics/stats.go`**:
  - Implement `CalculateGini(counts []int) float64`.
  - Implement `CalculateParetoTop20(counts []int) float64` (Sort descending, sum top 20% of array length, divide by total sum).
  - Implement `CalculateZScore(currentVolume int, historicalMean float64, historicalStdDev float64) float64`.
- **`/db/schema.sql` & `/db/sqlite.go`**:
  - Write a SQLite schema `channel_metrics` with columns: `date`, `channel_id`, `gini_coeff`, `pareto_share`, `z_score`, `dm_share_pct`, `avg_word_count`. Write insert/select queries.
- **`/api/server.go`**: 
  - Standard `net/http` server listening for Slack Event Webhooks. Route payloads into the `pipeline.go` engine.
- **`/mcp/mcp_server.go`**: 
  - Implement the Model Context Protocol. Expose ONE tool: `get_channel_burnout_metrics(channel_id string, date string)`. It returns the JSON row from SQLite.

---

## TASK 2: Generate the Slack Agent Frontend (`/slack-agent`)
This runs on Deno (TypeScript). Assume `@slack/deno-slack-sdk` is installed.

- **`manifest.ts`**:
  - Define the Slack App. Include required scopes: `channels:history`, `groups:history`, `chat:write`, `im:history`.
- **`/functions/evaluate_metrics.ts`**:
  - Write the logic that receives the JSON from the MCP server and uses `if/else` statements to check the math against the 3 "Burnout Indicators" defined above.
  - Determine which Risk Profile (if any) is currently active.
- **`/prompts/agent_context.ts`**:
  - Write the logic to read `agents.md` from disk.
  - Build the system prompt. **EXPLICIT REQUIREMENT:** You must configure the LLM client to strictly use Anthropic API calls with the `claude-3-5-sonnet-20240620` model.
- **`/ui/block_kit_builder.ts` (The Frontend UI)**:
  - Do not use raw Markdown for the final Slack message. The Agent must output structured Block Kit JSON.
  - Implement a function `buildAlertDashboard(riskProfile, llmSummary, suggestedAction)` that returns an array of Block Kit blocks:
    1. A `Header` block stating the channel name and Date.
    2. A `Section` block with an accessory Image/Icon (Yellow warning for moderate, Red for severe risk).
    3. A `Context` block containing the empathetic LLM-generated summary (The LLM output goes here).
    4. An `Actions` block containing a primary `Button` mapped to the `suggestedAction` (e.g., "Draft Anonymous Pulse Survey" or "Suggest No-Meeting Day").
- **`/functions/action_handlers.ts` (Interactivity):**
  - Implement Slack Block Action handlers for the interactive buttons generated in the UI.
  - Listen for the `action_id` named `trigger_pulse_survey`. When triggered, the handler must `ack()` the request, and use the Slack API to post an anonymous Block Kit poll/survey into the target channel.
  - Listen for the `action_id` named `suggest_no_meeting`. When triggered, the handler must `ack()` the request, and send an ephemeral message to the manager providing a pre-written draft they can copy/paste to their team.

## OUTPUT REQUIREMENTS
- Output the exact, complete code for all requested files.
- Add concise inline comments explaining how the Zero-Knowledge rules are being enforced at the memory level.
- Ensure all Go code has explicit error handling.
