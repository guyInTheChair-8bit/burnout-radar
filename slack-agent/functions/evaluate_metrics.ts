/**
 * functions/evaluate_metrics.ts
 *
 * Core burnout detection engine for BurnoutRadar.
 *
 * Implements three clinically-inspired burnout indicator rules using
 * aggregate communication-pattern metrics. NO individual message content
 * is ever read or stored — all analysis is done on anonymised metadata.
 *
 * Indicator Rules (exact thresholds — do not modify without updating agents.md):
 *  1. Key Person Dependency Risk   – Z-Score > 2.0 AND Gini > 0.7 AND Pareto > 85%
 *  2. Systemic Crunch Time Risk    – Z-Score > 2.0 AND Gini < 0.4 AND Sent/Rcv < 0.7
 *  3. Silent Isolation Risk        – Z-Score < 1.0 AND DM-shift > 70% AND word-count drop > 30%
 */

import { DefineFunction, Schema } from "https://deno.land/x/deno_slack_sdk@2.1.4/mod.ts";

// ---------------------------------------------------------------------------
// Domain types
// ---------------------------------------------------------------------------

/**
 * All aggregate, anonymised metrics collected for a single channel over a
 * rolling 7-day analysis window.
 */
export interface BurnoutMetrics {
  /** Slack channel ID (e.g. C012AB3CD) */
  channel_id: string;
  /** Human-readable channel name for display purposes */
  channel_name: string;
  /** ISO-8601 date string representing the analysis date (YYYY-MM-DD) */
  date: string;

  // ── Statistical activity indicators ──────────────────────────────────────
  /**
   * Z-score of message-volume for this channel vs. its own 30-day baseline.
   * Positive values indicate above-average activity; negative indicate below.
   */
  z_score: number;

  /**
   * Gini coefficient of message distribution across participants (0–1).
   * 0 = perfectly equal contribution; 1 = one person sends everything.
   * A high Gini (>0.7) combined with high Z-score signals key-person risk.
   * A low Gini (<0.4) combined with high Z-score signals team-wide crunch.
   */
  gini_coeff: number;

  /**
   * Percentage of total messages sent by the top-20% most active senders.
   * Named after the Pareto principle. Values >85% indicate heavy concentration.
   */
  pareto_top_20_share: number;

  /**
   * Ratio of messages sent BY the channel's primary contributor to messages
   * received FROM others. <0.7 during high-volume periods suggests the team
   * is consuming but not reciprocating — a crunch-time signal.
   */
  sent_received_ratio: number;

  /**
   * Percentage of recent activity that has shifted to direct messages (DMs)
   * away from public/group channels. Computed as:
   *   dm_messages / (dm_messages + public_messages) * 100
   */
  dm_share_pct: number;

  /**
   * Average word count per message in the current analysis window.
   * A significant drop from baseline can indicate disengagement or exhaustion.
   */
  avg_word_count: number;

  /**
   * 30-day rolling baseline for avg_word_count in this channel.
   * Used to compute relative word-count decline for the silent-isolation check.
   */
  avg_word_count_baseline: number;
}

/**
 * The four possible burnout risk profiles that BurnoutRadar can surface.
 *
 * 'key_person_dependency' – One or a few individuals driving ALL activity.
 * 'systemic_crunch_time'  – Whole team is overloaded; messages flood inbound.
 * 'silent_isolation'      – Activity has dropped AND shifted to private DMs.
 * 'none'                  – Metrics are within healthy operating parameters.
 */
export type RiskProfile =
  | "key_person_dependency"
  | "systemic_crunch_time"
  | "silent_isolation"
  | "none";

/** Severity classification for alerting purposes. */
export type Severity = "severe" | "moderate" | "none";

/** Output shape of evaluateMetrics(). */
export interface EvaluationResult {
  riskProfile: RiskProfile;
  severity: Severity;
}

// ---------------------------------------------------------------------------
// Burnout evaluation logic
// ---------------------------------------------------------------------------

/**
 * evaluateMetrics
 *
 * Applies the three burnout indicator rules (in priority order) to the
 * supplied aggregate metrics and returns the detected risk profile plus
 * a severity grade.
 *
 * Priority order matters: if a channel hits multiple rule thresholds
 * simultaneously, the first matching rule wins (key_person > crunch > isolation).
 *
 * @param metrics - Anonymised aggregate metrics for a channel analysis window.
 * @returns       - { riskProfile, severity }
 */
export function evaluateMetrics(metrics: BurnoutMetrics): EvaluationResult {
  const {
    z_score,
    gini_coeff,
    pareto_top_20_share,
    sent_received_ratio,
    dm_share_pct,
    avg_word_count,
    avg_word_count_baseline,
  } = metrics;

  // ── Rule 1: Key Person Dependency Risk ────────────────────────────────────
  // A single contributor (or tiny group) is driving an outsized share of
  // ALL messages in the channel during a spike period.
  //
  // Thresholds:
  //   • Z-Score > 2.0         → Channel is 2 standard deviations above its norm
  //   • Gini Coeff > 0.7      → Highly unequal message distribution
  //   • Pareto top-20% > 85%  → Top quintile sends >85% of all messages
  if (
    z_score > 2.0 &&
    gini_coeff > 0.7 &&
    pareto_top_20_share > 85
  ) {
    // Classify as 'severe' when the Gini is extremely high (near monopoly)
    const severity: Severity = gini_coeff > 0.85 ? "severe" : "moderate";
    return { riskProfile: "key_person_dependency", severity };
  }

  // ── Rule 2: Systemic Crunch Time Risk ─────────────────────────────────────
  // Message volume is surging across the whole team (low Gini → spread evenly)
  // but the primary contributor is mostly receiving rather than initiating,
  // which signals reactive fire-fighting rather than structured work.
  //
  // Thresholds:
  //   • Z-Score > 2.0         → Channel volume spike
  //   • Gini Coeff < 0.4      → Activity spread across many participants
  //   • Sent/Received < 0.7   → Team is consuming > producing messages
  if (
    z_score > 2.0 &&
    gini_coeff < 0.4 &&
    sent_received_ratio < 0.7
  ) {
    // Classify as 'severe' when sent/received ratio falls below 0.5 (heavy imbalance)
    const severity: Severity = sent_received_ratio < 0.5 ? "severe" : "moderate";
    return { riskProfile: "systemic_crunch_time", severity };
  }

  // ── Rule 3: Silent Isolation Risk ─────────────────────────────────────────
  // Overall activity is LOW, but the team has shifted toward private DMs and
  // messages have become much shorter — classic early-stage disengagement.
  //
  // Thresholds:
  //   • Z-Score < 1.0         → Channel volume is at or below baseline
  //   • DM share > 70%        → 70%+ of activity now happens in DMs
  //   • Word-count drop > 30% → Messages are ≥30% shorter than baseline
  //
  // Word-count decline formula:
  //   decline = (baseline - current) / baseline
  //   We flag when decline > 0.30  (i.e., >30% shorter messages)
  const wordCountDecline =
    avg_word_count_baseline > 0
      ? (avg_word_count_baseline - avg_word_count) / avg_word_count_baseline
      : 0;

  if (
    z_score < 1.0 &&
    dm_share_pct > 70 &&
    wordCountDecline > 0.30
  ) {
    // Classify as 'severe' when DM share is very high (>85%) or word-count
    // has collapsed by more than 50%
    const severity: Severity =
      dm_share_pct > 85 || wordCountDecline > 0.5 ? "severe" : "moderate";
    return { riskProfile: "silent_isolation", severity };
  }

  // ── No risk detected ──────────────────────────────────────────────────────
  return { riskProfile: "none", severity: "none" };
}

// ---------------------------------------------------------------------------
// SlackFunction definition (registered in manifest.ts)
// ---------------------------------------------------------------------------

/**
 * EvaluateMetricsFunction
 *
 * Wraps evaluateMetrics() as a first-class Slack Platform Function so it can
 * be composed into Workflows and triggered on a schedule or via event trigger.
 *
 * Input:  metrics_json – JSON-serialised BurnoutMetrics object
 * Output: risk_profile  – One of the four RiskProfile string literals
 *         severity      – 'severe' | 'moderate' | 'none'
 */
export const EvaluateMetricsFunction = DefineFunction({
  callback_id: "evaluate_metrics",
  title: "Evaluate Burnout Metrics",
  description:
    "Applies the three BurnoutRadar indicator rules to anonymised channel metrics " +
    "and returns the detected risk profile and severity.",
  source_file: "functions/evaluate_metrics.ts",
  input_parameters: {
    properties: {
      metrics_json: {
        type: Schema.types.string,
        description: "JSON-serialised BurnoutMetrics object for the analysis window.",
      },
    },
    required: ["metrics_json"],
  },
  output_parameters: {
    properties: {
      risk_profile: {
        type: Schema.types.string,
        description: "Detected risk profile: key_person_dependency | systemic_crunch_time | silent_isolation | none",
      },
      severity: {
        type: Schema.types.string,
        description: "Alert severity: severe | moderate | none",
      },
    },
    required: ["risk_profile", "severity"],
  },
});

/**
 * SlackFunction handler — invoked by the Slack Platform runtime.
 *
 * Deserialises the incoming JSON, calls evaluateMetrics(), and returns
 * the structured output back to the Workflow.
 */
export default async function evaluateMetricsHandler(
  { inputs }: { inputs: { metrics_json: string } },
) {
  // Parse the incoming JSON payload; surface a clear error if malformed
  let metrics: BurnoutMetrics;
  try {
    metrics = JSON.parse(inputs.metrics_json) as BurnoutMetrics;
  } catch (err) {
    throw new Error(`evaluate_metrics: failed to parse metrics_json — ${(err as Error).message}`);
  }

  const result = evaluateMetrics(metrics);

  return {
    outputs: {
      risk_profile: result.riskProfile,
      severity: result.severity,
    },
  };
}
