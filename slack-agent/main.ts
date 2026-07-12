/**
 * main.ts
 *
 * BurnoutRadar Slack Agent — main HTTP entry point.
 *
 * Responsibilities:
 *   • Serve an HTTP server using Deno.serve on the configured PORT
 *   • Verify Slack request signatures for all incoming webhooks
 *   • Route Slack interactivity payloads (block_actions) to action_handlers
 *   • Route Slack event callbacks (message/app_mention) through the full
 *     evaluate → Gemini → alert pipeline
 *   • Route POST /slack/commands for the /burnout slash command
 *   • Expose a /health endpoint for uptime monitoring
 *
 * Environment variables (required):
 *   SLACK_BOT_TOKEN      – Bot OAuth token (xoxb-...)
 *   SLACK_SIGNING_SECRET – Used to verify request signatures from Slack
 *   GEMINI_API_KEY       – Passed through to agent_context.ts
 *
 * Optional:
 *   PORT                 – HTTP port to listen on (default: 8080)
 *   BURNOUT_ALERTS_CHANNEL – Channel ID to post alerts to (default: channel from event)
 *   GO_DAEMON_URL        – Base URL of the Go MCP daemon (default: http://localhost:8080)
 */

import { evaluateMetrics, type BurnoutMetrics } from "./functions/evaluate_metrics.ts";
import { callGeminiForSummary } from "./prompts/agent_context.ts";
import {
  buildAlertDashboard,
  buildNoBurnoutMessage,
} from "./ui/block_kit_builder.ts";
import {
  dispatchAction,
  type BlockActionPayload,
} from "./functions/action_handlers.ts";

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

const PORT = parseInt(Deno.env.get("PORT") ?? "8080", 10);
const SLACK_BOT_TOKEN = Deno.env.get("SLACK_BOT_TOKEN") ?? "";
const SLACK_SIGNING_SECRET = Deno.env.get("SLACK_SIGNING_SECRET") ?? "";
const ALERTS_CHANNEL = Deno.env.get("BURNOUT_ALERTS_CHANNEL") ?? "";

// Base URL of the Go MCP daemon — used by the /burnout slash command handler
// to pull the latest channel metrics on demand.
const GO_DAEMON_URL = Deno.env.get("GO_DAEMON_URL") ?? "http://localhost:8080";

if (!SLACK_BOT_TOKEN || !SLACK_SIGNING_SECRET) {
  console.error(
    "[main] SLACK_BOT_TOKEN and SLACK_SIGNING_SECRET must be set. Exiting.",
  );
  Deno.exit(1);
}

// ---------------------------------------------------------------------------
// Slack request signature verification
// ---------------------------------------------------------------------------

/**
 * verifySlackSignature
 *
 * Validates that an incoming request genuinely originates from Slack by
 * recomputing the HMAC-SHA256 signature using the signing secret and
 * comparing it to the X-Slack-Signature header.
 *
 * Spec: https://api.slack.com/authentication/verifying-requests-from-slack
 *
 * @param request - The raw incoming Request object
 * @param body    - The raw request body string (must be read before calling)
 * @returns       - true if signature is valid, false otherwise
 */
async function verifySlackSignature(
  request: Request,
  body: string,
): Promise<boolean> {
  const slackSignature = request.headers.get("X-Slack-Signature");
  const slackTimestamp = request.headers.get("X-Slack-Request-Timestamp");

  if (!slackSignature || !slackTimestamp) {
    console.warn("[main] Missing Slack signature headers.");
    return false;
  }

  // Reject requests older than 5 minutes to prevent replay attacks
  const nowSeconds = Math.floor(Date.now() / 1000);
  if (Math.abs(nowSeconds - parseInt(slackTimestamp, 10)) > 300) {
    console.warn("[main] Slack request timestamp is too old — possible replay attack.");
    return false;
  }

  // Build the basestring that Slack signs
  const baseString = `v0:${slackTimestamp}:${body}`;

  // Import the signing secret as a HMAC-SHA256 key
  const encoder = new TextEncoder();
  const keyMaterial = await crypto.subtle.importKey(
    "raw",
    encoder.encode(SLACK_SIGNING_SECRET),
    { name: "HMAC", hash: "SHA-256" },
    false,
    ["sign"],
  );

  // Compute our own signature
  const signatureBuffer = await crypto.subtle.sign(
    "HMAC",
    keyMaterial,
    encoder.encode(baseString),
  );

  // Convert to hex string with 'v0=' prefix to match Slack's format
  const computedSignature =
    "v0=" +
    Array.from(new Uint8Array(signatureBuffer))
      .map((b) => b.toString(16).padStart(2, "0"))
      .join("");

  // Constant-time comparison to prevent timing attacks
  return timingSafeEqual(computedSignature, slackSignature);
}

/**
 * timingSafeEqual
 *
 * Compares two strings in constant time to avoid timing side-channels.
 */
function timingSafeEqual(a: string, b: string): boolean {
  if (a.length !== b.length) return false;
  let result = 0;
  for (let i = 0; i < a.length; i++) {
    result |= a.charCodeAt(i) ^ b.charCodeAt(i);
  }
  return result === 0;
}

// ---------------------------------------------------------------------------
// Slack API helpers
// ---------------------------------------------------------------------------

/** Posts a message to a Slack channel via chat.postMessage. */
async function postToSlack(
  channelId: string,
  text: string,
  blocks: unknown[],
): Promise<void> {
  const response = await fetch("https://slack.com/api/chat.postMessage", {
    method: "POST",
    headers: {
      "Content-Type": "application/json; charset=utf-8",
      Authorization: `Bearer ${SLACK_BOT_TOKEN}`,
    },
    body: JSON.stringify({ channel: channelId, text, blocks }),
  });

  const data = await response.json();
  if (!data.ok) {
    console.error(`[main] chat.postMessage failed: ${data.error}`);
  }
}

/**
 * postEphemeral
 *
 * Posts a Block Kit message to a channel via chat.postEphemeral so that
 * ONLY the specified user_id can see it. Used exclusively by the /burnout
 * slash command handler to deliver on-demand reports privately.
 */
async function postEphemeral(
  channelId: string,
  userId: string,
  blocks: unknown[],
  fallbackText: string,
): Promise<void> {
  const response = await fetch("https://slack.com/api/chat.postEphemeral", {
    method: "POST",
    headers: {
      "Content-Type": "application/json; charset=utf-8",
      Authorization: `Bearer ${SLACK_BOT_TOKEN}`,
    },
    body: JSON.stringify({
      channel: channelId,
      user: userId,
      text: fallbackText,
      blocks: blocks.length > 0 ? blocks : undefined,
    }),
  });

  const data = await response.json();
  if (!data.ok) {
    console.error(`[main] chat.postEphemeral failed: ${data.error}`);
  }
}

// ---------------------------------------------------------------------------
// Full burnout detection pipeline
// ---------------------------------------------------------------------------

/**
 * runBurnoutPipeline
 *
 * Executes the full evaluation pipeline for a set of metrics:
 *   1. evaluateMetrics()       – Applies indicator rules
 *   2. callGeminiForSummary()  – Generates LLM narrative
 *   3. buildAlertDashboard()   – Constructs Block Kit payload
 *   4. postToSlack()           – Delivers the alert to the channel
 *
 * @param metrics   - Anonymised aggregate channel metrics
 * @param channelId - Destination channel for the alert
 */
async function runBurnoutPipeline(
  metrics: BurnoutMetrics,
  channelId: string,
): Promise<void> {
  console.log(
    `[main] Running burnout pipeline for #${metrics.channel_name} on ${metrics.date}`,
  );

  // Step 1: Evaluate metrics against the three indicator rules
  const { riskProfile, severity } = evaluateMetrics(metrics);
  console.log(`[main] Risk profile: ${riskProfile} (${severity})`);

  // Step 2: If no risk, post the healthy check-in message and return early
  if (riskProfile === "none") {
    const blocks = buildNoBurnoutMessage(metrics.channel_name, metrics.date);
    await postToSlack(channelId, "✅ BurnoutRadar: All healthy!", blocks);
    return;
  }

  // Step 3: Generate LLM summary via Gemini
  let llmSummary = "Communication patterns suggest potential team stress.";
  let suggestedAction = "Consider a team check-in to discuss workload balance.";

  try {
    const llmResult = await callGeminiForSummary(riskProfile, metrics);
    llmSummary = llmResult.summary;
    suggestedAction = llmResult.suggestedAction;
  } catch (err) {
    // Non-fatal: log and continue with fallback text
    console.error(
      `[main] Gemini API call failed, using fallback text: ${(err as Error).message}`,
    );
  }

  // Step 4: Build the Block Kit alert dashboard
  const blocks = buildAlertDashboard(
    metrics.channel_name,
    metrics.date,
    riskProfile,
    severity,
    llmSummary,
    suggestedAction,
  );

  // Step 5: Post the alert
  await postToSlack(
    channelId,
    `${severity === "severe" ? "🚨" : "⚠️"} BurnoutRadar Alert: ${riskProfile}`,
    blocks,
  );

  console.log(`[main] Alert posted to ${channelId}`);
}

// ---------------------------------------------------------------------------
// /burnout slash command handler
// ---------------------------------------------------------------------------

/**
 * processSlashCommand
 *
 * Async background processor for the /burnout Slack slash command.
 * Runs AFTER the 200 OK has already been sent to Slack (within 3s limit).
 *
 * Flow:
 *   1. Fetch the latest channel metrics from the Go daemon's GET /api/metrics
 *   2. Evaluate the risk profile with evaluateMetrics()
 *   3. Call callGeminiForSummary() for the LLM narrative
 *   4. Build Block Kit with buildAlertDashboard() or buildNoBurnoutMessage()
 *   5. Post the result via chat.postEphemeral — visible ONLY to the invoking manager
 */
async function processSlashCommand(
  channelId: string,
  channelName: string,
  userId: string,
): Promise<void> {
  console.log(
    `[slash] /burnout invoked by user=${userId} in channel=${channelId} (#${channelName})`,
  );

  // ── Step 1: Pull latest metrics from the Go daemon ───────────────────────
  let metrics: BurnoutMetrics;
  try {
    const url = `${GO_DAEMON_URL}/api/metrics?channel_id=${encodeURIComponent(channelId)}`;
    const resp = await fetch(url, {
      signal: AbortSignal.timeout(8_000), // 8 s — daemon should respond quickly
    });

    if (resp.status === 404) {
      // Channel registered but no evaluation tick has run yet.
      await postEphemeral(
        channelId,
        userId,
        [],
        `⏳ BurnoutRadar hasn't computed metrics for *#${channelName}* yet. ` +
          "Metrics are calculated on each evaluation tick (every 60 s by default). " +
          "Please try again shortly.",
      );
      return;
    }

    if (!resp.ok) {
      throw new Error(`Go daemon returned HTTP ${resp.status}`);
    }

    metrics = (await resp.json()) as BurnoutMetrics;
  } catch (err) {
    console.error(`[slash] daemon fetch failed: ${(err as Error).message}`);
    await postEphemeral(
      channelId,
      userId,
      [],
      "⚠️ BurnoutRadar could not reach the metrics daemon. Is it running? " +
        `(${(err as Error).message})`,
    );
    return;
  }

  // ── Step 2: Evaluate risk profile against the three burnout rules ─────────
  const { riskProfile, severity } = evaluateMetrics(metrics);
  console.log(`[slash] channel=#${channelName} risk=${riskProfile} severity=${severity}`);

  // ── Step 3: Generate LLM narrative via Gemini ─────────────────────────────
  let llmSummary = "Communication patterns show aggregated activity for this channel.";
  let suggestedAction = "Continue monitoring — no immediate action required.";

  if (riskProfile !== "none") {
    try {
      const llmResult = await callGeminiForSummary(riskProfile, metrics);
      llmSummary = llmResult.summary;
      suggestedAction = llmResult.suggestedAction;
    } catch (err) {
      // Non-fatal — fallback text is already set above.
      console.error(`[slash] Gemini failed: ${(err as Error).message}`);
    }
  }

  // ── Step 4: Build Block Kit blocks ───────────────────────────────────────
  const blocks =
    riskProfile === "none"
      ? buildNoBurnoutMessage(channelName, metrics.date)
      : buildAlertDashboard(
          channelName,
          metrics.date,
          riskProfile,
          severity,
          llmSummary,
          suggestedAction,
        );

  // ── Step 5: Deliver privately to the invoking manager only ───────────────
  const fallback =
    riskProfile === "none"
      ? `✅ BurnoutRadar: #${channelName} looks healthy.`
      : `${severity === "severe" ? "🚨" : "⚠️"} BurnoutRadar: ${riskProfile} detected in #${channelName}.`;

  await postEphemeral(channelId, userId, blocks, fallback);
  console.log(`[slash] ephemeral report delivered to user=${userId} in channel=${channelId}`);
}

// ---------------------------------------------------------------------------
// HTTP request handler
// ---------------------------------------------------------------------------

/**
 * handleRequest
 *
 * Main request dispatcher. Handles:
 *   GET  /health           – Uptime check
 *   POST /slack/events     – Slack Event API callbacks
 *   POST /slack/actions    – Slack interactivity payloads (block_actions)
 *   POST /slack/commands   – Slack slash commands (/burnout)
 *   POST /metrics          – Internal metrics injection from the Go daemon
 */
async function handleRequest(request: Request): Promise<Response> {
  const url = new URL(request.url);

  // ── Health check ──────────────────────────────────────────────────────────
  if (request.method === "GET" && url.pathname === "/health") {
    return new Response(
      JSON.stringify({ status: "ok", service: "burnout-radar-slack-agent" }),
      { status: 200, headers: { "Content-Type": "application/json" } },
    );
  }

  // ── Slash command endpoint (/burnout) ─────────────────────────────────────
  // Slack sends slash commands as application/x-www-form-urlencoded POST.
  // We MUST respond within 3 seconds — ack immediately, then process async.
  if (request.method === "POST" && url.pathname === "/slack/commands") {
    const isValid = await verifySlackSignature(request, await request.clone().text());
    if (!isValid) {
      return new Response("Unauthorized", { status: 401 });
    }

    const formData = await request.formData();
    const channelId = (formData.get("channel_id") as string) ?? "";
    const channelName = (formData.get("channel_name") as string) ?? channelId;
    const userId = (formData.get("user_id") as string) ?? "";
    const command = (formData.get("command") as string) ?? "";

    if (!channelId || !userId) {
      return new Response(
        JSON.stringify({ response_type: "ephemeral", text: "⚠️ Could not identify channel or user." }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      );
    }

    console.log(`[slash] received ${command} from user=${userId} in channel=${channelId}`);

    // Fire-and-forget: process asynchronously after acknowledging Slack.
    processSlashCommand(channelId, channelName, userId).catch((err) =>
      console.error(`[slash] unhandled error: ${(err as Error).message}`)
    );

    // Immediate 200 OK acknowledgment — Slack requires this within 3 seconds.
    return new Response(
      JSON.stringify({
        response_type: "ephemeral",
        text: `⏳ Fetching BurnoutRadar report for *#${channelName}*\u2026`,
      }),
      { status: 200, headers: { "Content-Type": "application/json" } },
    );
  }

  // All other routes require POST
  if (request.method !== "POST") {
    return new Response("Method Not Allowed", { status: 405 });
  }

  // Read the raw body once (needed for both signature verification and parsing)
  const rawBody = await request.text();

  // ── Slack events endpoint ─────────────────────────────────────────────────
  if (url.pathname === "/slack/events") {
    // Verify the request came from Slack
    const isValid = await verifySlackSignature(request, rawBody);
    if (!isValid) {
      return new Response("Unauthorized", { status: 401 });
    }

    let payload: Record<string, unknown>;
    try {
      payload = JSON.parse(rawBody);
    } catch {
      return new Response("Bad Request: invalid JSON", { status: 400 });
    }

    // Handle Slack's url_verification challenge (sent once when configuring the endpoint)
    if (payload.type === "url_verification") {
      return new Response(
        JSON.stringify({ challenge: payload.challenge }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      );
    }

    // Handle app_home_opened or message events (fire-and-forget, respond 200 immediately)
    if (payload.type === "event_callback") {
      const event = payload.event as Record<string, unknown>;
      const channelId =
        (event.channel as string) ?? ALERTS_CHANNEL;

      // For demonstration: if the event carries a metrics payload in the metadata,
      // extract it and run the pipeline. In production, metrics would be injected
      // via the /metrics endpoint from the Python analytics backend.
      if (event.type === "app_mention" && event.text) {
        const text = event.text as string;
        // Allow inline JSON metrics for debugging: @BurnoutRadar {...metrics json...}
        const jsonMatch = text.match(/\{[\s\S]+\}/);
        if (jsonMatch) {
          try {
            const metrics = JSON.parse(jsonMatch[0]) as BurnoutMetrics;
            // Run asynchronously — don't block the 200 OK response
            runBurnoutPipeline(metrics, channelId).catch(console.error);
          } catch {
            console.warn("[main] Could not parse inline metrics JSON from mention.");
          }
        }
      }
    }

    return new Response("", { status: 200 });
  }

  // ── Slack interactivity endpoint ──────────────────────────────────────────
  if (url.pathname === "/slack/actions") {
    const isValid = await verifySlackSignature(request, rawBody);
    if (!isValid) {
      return new Response("Unauthorized", { status: 401 });
    }

    // Slack sends interactivity payloads as URL-encoded `payload=<json>`
    const params = new URLSearchParams(rawBody);
    const payloadJson = params.get("payload");

    if (!payloadJson) {
      return new Response("Bad Request: missing payload", { status: 400 });
    }

    let actionPayload: BlockActionPayload;
    try {
      actionPayload = JSON.parse(payloadJson) as BlockActionPayload;
    } catch {
      return new Response("Bad Request: invalid payload JSON", { status: 400 });
    }

    if (actionPayload.type !== "block_actions") {
      // Only handle block_actions; other interactivity types (shortcuts, modals) ignored
      return new Response("", { status: 200 });
    }

    // Dispatch the action asynchronously so Slack's 3-second timeout is respected
    dispatchAction(actionPayload, SLACK_BOT_TOKEN).catch((err) =>
      console.error(`[main] dispatchAction error: ${(err as Error).message}`)
    );

    // Respond immediately with 200 OK (Slack requires response within 3 seconds)
    return new Response("", { status: 200 });
  }

  // ── Internal metrics injection endpoint ───────────────────────────────────
  // Used by the Python analytics backend to push computed metrics for evaluation.
  if (url.pathname === "/metrics") {
    // Basic bearer token auth for internal calls
    const authHeader = request.headers.get("Authorization") ?? "";
    const expectedToken = `Bearer ${SLACK_BOT_TOKEN}`;
    if (authHeader !== expectedToken) {
      return new Response("Unauthorized", { status: 401 });
    }

    let body: { metrics: BurnoutMetrics; channel_id?: string };
    try {
      body = JSON.parse(rawBody);
    } catch {
      return new Response("Bad Request: invalid JSON", { status: 400 });
    }

    const targetChannel = body.channel_id ?? body.metrics.channel_id ?? ALERTS_CHANNEL;

    if (!targetChannel) {
      return new Response(
        "Bad Request: channel_id required (set BURNOUT_ALERTS_CHANNEL or include in body)",
        { status: 400 },
      );
    }

    // Run pipeline asynchronously
    runBurnoutPipeline(body.metrics, targetChannel).catch((err) =>
      console.error(`[main] Pipeline error: ${(err as Error).message}`)
    );

    return new Response(
      JSON.stringify({ accepted: true }),
      { status: 202, headers: { "Content-Type": "application/json" } },
    );
  }

  return new Response("Not Found", { status: 404 });
}

// ---------------------------------------------------------------------------
// Server bootstrap
// ---------------------------------------------------------------------------

console.log(`[main] BurnoutRadar Slack Agent starting on port ${PORT} …`);

Deno.serve({ port: PORT }, handleRequest);
