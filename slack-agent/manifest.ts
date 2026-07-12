/**
 * manifest.ts
 *
 * Defines the BurnoutRadar Slack App manifest using @slack/deno-slack-sdk.
 * Registers OAuth scopes, custom functions, and outgoing domains needed
 * for the privacy-first burnout detection agent.
 */

import { Manifest } from "https://deno.land/x/deno_slack_sdk@2.1.4/mod.ts";
import { EvaluateMetricsFunction } from "./functions/evaluate_metrics.ts";
import { ActionHandlersFunction } from "./functions/action_handlers.ts";

export default Manifest({
  name: "BurnoutRadar",
  description:
    "Privacy-first team burnout detection. Surfaces aggregate communication patterns " +
    "to help managers take early, supportive action — without accessing any individual messages.",
  icon: "assets/icon.png",

  /**
   * OAuth Bot Token Scopes required by BurnoutRadar.
   *
   * channels:history  – Read public channel message counts/metadata for metrics
   * groups:history    – Read private channel message counts/metadata
   * im:history        – Read DM metadata (aggregate only, no content)
   * chat:write        – Post alert dashboards and ephemeral messages
   * chat:write.public – Post to channels the bot hasn't explicitly joined
   * commands          – Support slash commands if extended later
   */
  botScopes: [
    "channels:history",
    "groups:history",
    "im:history",
    "chat:write",
    "chat:write.public",
    "commands",
  ],

  /**
   * Custom SlackFunctions registered with the platform.
   * evaluate_metrics – Applies burnout indicator rules to channel metrics.
   * action_handlers  – Handles Block Kit interactive button payloads.
   */
  functions: [EvaluateMetricsFunction, ActionHandlersFunction],

  datastores: [],
  workflows: [],

  /**
   * Outgoing domains the app may call via fetch().
   * generativelanguage.googleapis.com is required for Gemini summary generation.
   */
  outgoingDomains: ["generativelanguage.googleapis.com"],
});
