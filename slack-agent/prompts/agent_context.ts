/**
 * prompts/agent_context.ts
 *
 * Responsible for loading the BurnoutRadar agent persona and system prompt
 * from the agents.md specification file, and making Gemini API calls to 
 * generate human-friendly summaries for detected burnout signals.
 */

import type { BurnoutMetrics, RiskProfile } from "../functions/evaluate_metrics.ts";

export interface LLMSummaryResult {
  summary: string;
  suggestedAction: string;
}

// ---------------------------------------------------------------------------
// System prompt loader
// ---------------------------------------------------------------------------

export async function buildSystemPrompt(): Promise<string> {
  const agentsPath =
    Deno.env.get("BURNOUT_AGENTS_MD_PATH") ??
    new URL("../../agents.md", import.meta.url).pathname;

  try {
    const content = await Deno.readTextFile(agentsPath);
    return content.trim();
  } catch (err) {
    console.warn(
      `[agent_context] Could not read agents.md at ${agentsPath}: ${(err as Error).message}. ` +
        "Using minimal fallback system prompt.",
    );

    return [
      "You are BurnoutRadar, a privacy-first AI assistant that helps team managers",
      "detect early signs of team burnout from anonymised aggregate communication patterns.",
      "You NEVER reference individual team members or their private messages.",
      "You speak with empathy, clarity, and actionable insight.",
      "Your responses should be concise (2–3 sentences for the summary,",
      "1 sentence for the suggested action), professional, and warm.",
    ].join(" ");
  }
}

// ---------------------------------------------------------------------------
// Gemini API caller
// ---------------------------------------------------------------------------

/**
 * callGeminiForSummary
 *
 * Calls the Google Gemini REST API to generate a human-readable manager
 * summary and a concrete suggested action for a detected burnout signal.
 */
export async function callGeminiForSummary(
  riskProfile: RiskProfile,
  metrics: BurnoutMetrics,
): Promise<LLMSummaryResult> {
  const apiKey = Deno.env.get("GEMINI_API_KEY");
  if (!apiKey) {
    throw new Error(
      "[agent_context] GEMINI_API_KEY environment variable is not set.",
    );
  }

  const systemPrompt = await buildSystemPrompt();
  const userMessage = buildUserMessage(riskProfile, metrics);

  // Using Gemini 1.5 Flash for high speed and low latency
//  const url = `https://generativelanguage.googleapis.com/v1beta/models/gemini-1.5-flash:generateContent?key=${apiKey}`;
const url = `https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent?key=${apiKey}`;
  const requestBody = {
    systemInstruction: {
      parts: [{ text: systemPrompt }]
    },
    contents: [
      {
        role: "user",
        parts: [{ text: userMessage }]
      }
    ],
    generationConfig: {
      temperature: 0.2, // Low temperature for consistent responses
      // Removed responseMimeType to prevent truncation issues
    }
  };

  let response: Response;
  try {
    response = await fetch(url, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
      },
      body: JSON.stringify(requestBody),
    });
  } catch (networkErr) {
    throw new Error(
      `[agent_context] Network error calling Gemini API: ${(networkErr as Error).message}`,
    );
  }

  if (!response.ok) {
    const errorText = await response.text().catch(() => "(unreadable body)");
    throw new Error(
      `[agent_context] Gemini API error ${response.status}: ${errorText}`,
    );
  }

  const data = await response.json();
  const rawText = data.candidates?.[0]?.content?.parts?.[0]?.text ?? "";

  if (!rawText) {
    throw new Error(
      "[agent_context] Gemini API returned an empty text response.",
    );
  }

  return parseLLMOutput(rawText);
}

// ---------------------------------------------------------------------------
// Private helpers
// ---------------------------------------------------------------------------

function buildUserMessage(riskProfile: RiskProfile, metrics: BurnoutMetrics): string {
  const riskLabels: Record<RiskProfile, string> = {
    key_person_dependency: "Key Person Dependency Risk",
    systemic_crunch_time: "Systemic Crunch Time Risk",
    silent_isolation: "Silent Isolation Risk",
    none: "No Risk Detected",
  };

  return [
    `BurnoutRadar has detected a "${riskLabels[riskProfile]}" pattern in the #${metrics.channel_name} channel.`,
    "",
    "Here are the anonymised aggregate metrics that triggered this alert:",
    JSON.stringify(
      {
        channel_name: metrics.channel_name,
        date: metrics.date,
        z_score: metrics.z_score,
        gini_coefficient: metrics.gini_coeff,
        pareto_top_20_share_pct: metrics.pareto_top_20_share,
        sent_received_ratio: metrics.sent_received_ratio,
        dm_share_pct: metrics.dm_share_pct,
        avg_word_count: metrics.avg_word_count,
        avg_word_count_baseline: metrics.avg_word_count_baseline,
      },
      null,
      2,
    ),
    "",
    "Please respond with ONLY a valid JSON object containing exactly two keys (no code fences, no preamble):",
    '  "summary": "<2–3 sentences: explain the pattern and its human significance>",',
    '  "suggestedAction": "<1 sentence: a concrete, empathetic next step the manager can take>"',
    "",
    "Remember: never reference individual team members. Frame everything around team health.",
  ].join("\n");
}

function parseLLMOutput(rawText: string): LLMSummaryResult {
  // Strip any accidental markdown fences Gemini might include
  const cleaned = rawText
    .replace(/^```(?:json)?\s*/i, "")
    .replace(/```\s*$/, "")
    .trim();

  try {
    const parsed = JSON.parse(cleaned) as {
      summary?: string;
      suggestedAction?: string;
    };

    return {
      summary: parsed.summary ?? cleaned,
      suggestedAction:
        parsed.suggestedAction ??
        "Consider scheduling a 1:1 with your team to check in on how everyone is doing.",
    };
  } catch {
    console.warn(
      "[agent_context] LLM response was not valid JSON — using raw text as summary.",
    );
    return {
      summary: cleaned,
      suggestedAction:
        "Consider scheduling a 1:1 with your team to check in on how everyone is doing.",
    };
  }
}
