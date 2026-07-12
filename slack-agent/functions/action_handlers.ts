/**
 * functions/action_handlers.ts
 *
 * Handles Slack Block Kit interactive button actions triggered from the
 * BurnoutRadar alert dashboard.
 *
 * Registered action_ids:
 *   trigger_pulse_survey    – Posts a 5-question anonymous pulse survey to the channel
 *   suggest_no_meeting      – Sends an ephemeral no-meeting-day draft to the triggering user
 *   draft_comp_time         – Sends an ephemeral comp-time reminder draft to the triggering user
 *   trigger_workload_review – Sends an ephemeral workload review prompt (key-person case)
 *
 * All handlers are designed to be idempotent and gracefully handle errors
 * without crashing the webhook server.
 */

import { DefineFunction, Schema } from "https://deno.land/x/deno_slack_sdk@2.1.4/mod.ts";

// ---------------------------------------------------------------------------
// SlackFunction definition (registered in manifest.ts)
// ---------------------------------------------------------------------------

/**
 * ActionHandlersFunction
 *
 * Registered as a Slack Platform Function so the platform knows this module
 * handles interactivity payloads. The actual routing logic lives in the
 * HTTP handler in main.ts which calls dispatchAction() directly.
 */
export const ActionHandlersFunction = DefineFunction({
  callback_id: "action_handlers",
  title: "BurnoutRadar Action Handlers",
  description:
    "Handles Block Kit button interactions from BurnoutRadar alert dashboards.",
  source_file: "functions/action_handlers.ts",
  input_parameters: {
    properties: {
      action_id: {
        type: Schema.types.string,
        description: "The action_id of the button that was clicked.",
      },
      channel_id: {
        type: Schema.types.string,
        description: "The channel where the action originated.",
      },
      user_id: {
        type: Schema.types.string,
        description: "The Slack user ID of the person who clicked the button.",
      },
      trigger_id: {
        type: Schema.types.string,
        description: "Slack trigger ID for opening modals if needed.",
      },
    },
    required: ["action_id", "channel_id", "user_id"],
  },
  output_parameters: {
    properties: {
      ok: {
        type: Schema.types.boolean,
        description: "Whether the action was handled successfully.",
      },
    },
    required: ["ok"],
  },
});

// ---------------------------------------------------------------------------
// Payload types (subset of the full Slack interactivity payload)
// ---------------------------------------------------------------------------

export interface BlockActionPayload {
  type: "block_actions";
  trigger_id: string;
  response_url?: string;
  channel?: { id: string; name: string };
  user: { id: string; username: string; name: string };
  actions: Array<{
    action_id: string;
    value?: string;
    block_id?: string;
  }>;
  message?: {
    ts: string;
    blocks?: unknown[];
  };
  container?: {
    channel_id?: string;
  };
}

/** Minimal Slack Web API response shape. */
interface SlackAPIResponse {
  ok: boolean;
  error?: string;
  ts?: string;
}

// ---------------------------------------------------------------------------
// Action dispatcher (called from main.ts)
// ---------------------------------------------------------------------------

/**
 * dispatchAction
 *
 * Routes an incoming block_actions payload to the appropriate handler
 * based on the action_id of the first triggered action.
 *
 * @param payload  - Parsed Slack block_actions payload
 * @param botToken - Slack bot token for making API calls
 * @returns        - void (handlers post to Slack directly)
 */
export async function dispatchAction(
  payload: BlockActionPayload,
  botToken: string,
): Promise<void> {
  const action = payload.actions[0];
  if (!action) {
    console.warn("[action_handlers] No actions in payload — skipping.");
    return;
  }

  // Derive the channel ID from wherever it's available in the payload
  const channelId =
    payload.channel?.id ??
    payload.container?.channel_id ??
    "";

  const userId = payload.user.id;

  console.log(`[action_handlers] Dispatching action: ${action.action_id} in ${channelId}`);

  switch (action.action_id) {
    case "trigger_pulse_survey":
      await handlePulseSurvey(channelId, botToken);
      break;

    case "suggest_no_meeting":
      await handleSuggestNoMeeting(channelId, userId, botToken);
      break;

    case "draft_comp_time":
      await handleDraftCompTime(channelId, userId, botToken);
      break;

    case "trigger_workload_review":
      await handleWorkloadReview(channelId, userId, botToken);
      break;

    default:
      console.warn(`[action_handlers] Unknown action_id: ${action.action_id}`);
  }
}

// ---------------------------------------------------------------------------
// Individual action handlers
// ---------------------------------------------------------------------------

/**
 * handlePulseSurvey
 *
 * Posts a 5-question anonymous emoji-scale pulse survey to the channel.
 * The survey uses Block Kit buttons so responses are ephemeral to each user
 * and no data is persisted (privacy-first principle).
 *
 * Questions:
 *   1. Team support feeling   (emoji scale: 😔 😐 🙂 😊 🤩)
 *   2. Workload manageability (traffic-light scale: 🔴 🟠 🟡 🟢 ✅)
 *   3. Goal connection        (number scale: 1️⃣–5️⃣)
 *   4. Overwhelm frequency    (text scale: Always–Never)
 *   5. Energy level           (colour-fill scale: ⬛–🟩)
 */
async function handlePulseSurvey(channelId: string, botToken: string): Promise<void> {
  const today = new Date().toLocaleDateString("en-US", {
    weekday: "long",
    month: "long",
    day: "numeric",
  });

  const blocks = [
    {
      type: "header",
      text: {
        type: "plain_text",
        text: "📋 Anonymous Team Pulse Survey",
        emoji: true,
      },
    },
    {
      type: "section",
      text: {
        type: "mrkdwn",
        text:
          `Hey team 👋 It's ${today}. Your manager cares about how you're doing. ` +
          "This is a *completely anonymous* pulse check — tap one emoji per question. " +
          "No names. No tracking. Just honest signals. 💙",
      },
    },
    { type: "divider" },

    // Question 1 — Team support
    {
      type: "section",
      text: {
        type: "mrkdwn",
        text: "*1️⃣  How supported do you feel by your team this week?*",
      },
    },
    {
      type: "actions",
      block_id: "q1_support",
      elements: buildEmojiButtons(
        [
          { emoji: "😔", label: "Not at all", value: "q1_1" },
          { emoji: "😐", label: "A little", value: "q1_2" },
          { emoji: "🙂", label: "Somewhat", value: "q1_3" },
          { emoji: "😊", label: "Quite a bit", value: "q1_4" },
          { emoji: "🤩", label: "Completely", value: "q1_5" },
        ],
        "pulse_q1",
      ),
    },

    // Question 2 — Workload manageability
    {
      type: "section",
      text: {
        type: "mrkdwn",
        text: "*2️⃣  How manageable is your current workload?*",
      },
    },
    {
      type: "actions",
      block_id: "q2_workload",
      elements: buildEmojiButtons(
        [
          { emoji: "🔴", label: "Overwhelming", value: "q2_1" },
          { emoji: "🟠", label: "Heavy", value: "q2_2" },
          { emoji: "🟡", label: "Balanced", value: "q2_3" },
          { emoji: "🟢", label: "Light", value: "q2_4" },
          { emoji: "✅", label: "Very light", value: "q2_5" },
        ],
        "pulse_q2",
      ),
    },

    // Question 3 — Goal connection
    {
      type: "section",
      text: {
        type: "mrkdwn",
        text: "*3️⃣  How connected do you feel to the team's goals?*",
      },
    },
    {
      type: "actions",
      block_id: "q3_goals",
      elements: buildEmojiButtons(
        [
          { emoji: "1️⃣", label: "Not connected", value: "q3_1" },
          { emoji: "2️⃣", label: "Slightly", value: "q3_2" },
          { emoji: "3️⃣", label: "Moderately", value: "q3_3" },
          { emoji: "4️⃣", label: "Strongly", value: "q3_4" },
          { emoji: "5️⃣", label: "Fully aligned", value: "q3_5" },
        ],
        "pulse_q3",
      ),
    },

    // Question 4 — Overwhelm frequency
    {
      type: "section",
      text: {
        type: "mrkdwn",
        text: "*4️⃣  How often do you feel overwhelmed?*",
      },
    },
    {
      type: "actions",
      block_id: "q4_overwhelm",
      elements: buildEmojiButtons(
        [
          { emoji: "🌊", label: "Always", value: "q4_1" },
          { emoji: "😓", label: "Often", value: "q4_2" },
          { emoji: "🤔", label: "Sometimes", value: "q4_3" },
          { emoji: "😌", label: "Rarely", value: "q4_4" },
          { emoji: "🧘", label: "Never", value: "q4_5" },
        ],
        "pulse_q4",
      ),
    },

    // Question 5 — Energy level
    {
      type: "section",
      text: {
        type: "mrkdwn",
        text: "*5️⃣  Overall, how is your energy level?*",
      },
    },
    {
      type: "actions",
      block_id: "q5_energy",
      elements: buildEmojiButtons(
        [
          { emoji: "⬛", label: "Depleted", value: "q5_1" },
          { emoji: "🟫", label: "Low", value: "q5_2" },
          { emoji: "🟧", label: "Medium", value: "q5_3" },
          { emoji: "🟨", label: "Good", value: "q5_4" },
          { emoji: "🟩", label: "Full", value: "q5_5" },
        ],
        "pulse_q5",
      ),
    },

    { type: "divider" },
    {
      type: "context",
      elements: [
        {
          type: "mrkdwn",
          text: "🔒 *Completely anonymous.* Individual responses are never stored or attributed.",
        },
      ],
    },
  ];

  await postMessage(channelId, botToken, {
    text: "📋 Anonymous Team Pulse Survey — tap to respond!",
    blocks,
  });
}

/**
 * handleSuggestNoMeeting
 *
 * Sends an ephemeral message to the triggering manager with a pre-written
 * no-meeting-day announcement draft they can edit and post.
 */
async function handleSuggestNoMeeting(
  channelId: string,
  userId: string,
  botToken: string,
): Promise<void> {
  // Compute a suggested date: next business day that is a Monday (ideal no-meeting day)
  const suggestedDate = getNextMonday();

  const draftMessage =
    `Hey team 👋 I wanted to share something important. ` +
    `I've been keeping an eye on our team's energy levels, and I think we could all use a breather. ` +
    `I'm declaring *${suggestedDate}* a *No-Meeting Day* for our team. ` +
    `Use this time for deep work, catching up, or just taking it a bit easier. ` +
    `You've all been working hard and I appreciate every one of you. — [Manager Name]`;

  const blocks = [
    {
      type: "header",
      text: { type: "plain_text", text: "📅 No-Meeting Day Draft", emoji: true },
    },
    {
      type: "section",
      text: {
        type: "mrkdwn",
        text:
          "Here's a message draft you can post to your team. Edit as needed, then share it in the channel! ✏️",
      },
    },
    {
      type: "section",
      text: {
        type: "mrkdwn",
        text: `> ${draftMessage}`,
      },
    },
    {
      type: "context",
      elements: [
        {
          type: "mrkdwn",
          text: "💡 _Only you can see this message. Copy the draft above and post it when you're ready._",
        },
      ],
    },
  ];

  await postEphemeral(channelId, userId, botToken, {
    text: "📅 No-Meeting Day draft ready for you!",
    blocks,
  });
}

/**
 * handleDraftCompTime
 *
 * Sends an ephemeral message to the triggering manager with a pre-written
 * comp time announcement draft.
 */
async function handleDraftCompTime(
  channelId: string,
  userId: string,
  botToken: string,
): Promise<void> {
  const draftMessage =
    `Hey team 🙌 You've absolutely crushed it during this sprint. ` +
    `As a thank-you, I'm giving everyone *[X] extra day(s)* of comp time to use anytime in the next 30 days. ` +
    `Please coordinate with your direct reports and block the time on your calendar. ` +
    `Thank you for everything you do. — [Manager Name]`;

  const blocks = [
    {
      type: "header",
      text: { type: "plain_text", text: "🎁 Comp Time Reminder Draft", emoji: true },
    },
    {
      type: "section",
      text: {
        type: "mrkdwn",
        text:
          "Here's a comp time message draft for your team. Fill in the number of days, then share it! ✏️",
      },
    },
    {
      type: "section",
      text: {
        type: "mrkdwn",
        text: `> ${draftMessage}`,
      },
    },
    {
      type: "context",
      elements: [
        {
          type: "mrkdwn",
          text: "💡 _Only you can see this message. Copy the draft above and customise before posting._",
        },
      ],
    },
  ];

  await postEphemeral(channelId, userId, botToken, {
    text: "🎁 Comp time draft ready!",
    blocks,
  });
}

/**
 * handleWorkloadReview
 *
 * Sends an ephemeral message to the triggering manager with a structured
 * workload review checklist for key-person-dependency situations.
 */
async function handleWorkloadReview(
  channelId: string,
  userId: string,
  botToken: string,
): Promise<void> {
  const blocks = [
    {
      type: "header",
      text: { type: "plain_text", text: "⚖️ Workload Allocation Review", emoji: true },
    },
    {
      type: "section",
      text: {
        type: "mrkdwn",
        text:
          "BurnoutRadar detected a *Key Person Dependency* pattern. " +
          "One or a few people may be carrying a disproportionate share of the team's communication load. " +
          "Here are some steps to help redistribute:",
      },
    },
    {
      type: "section",
      text: {
        type: "mrkdwn",
        text: [
          "• 📋 *Map current responsibilities* — review who owns what in your project tracker",
          "• 🤝 *Identify pairing opportunities* — pair the high-load person with a teammate on key tasks",
          "• 📅 *Schedule a 1:1* — check in privately on how they're coping with their current load",
          "• 🔄 *Rotate on-call or lead roles* — spread the spokesperson/coordinator burden",
          "• 📣 *Encourage whole-team contributions* — invite quieter team members into discussions",
        ].join("\n"),
      },
    },
    {
      type: "context",
      elements: [
        {
          type: "mrkdwn",
          text: "💡 _Only you can see this message. All insights are based on anonymised aggregate patterns._",
        },
      ],
    },
  ];

  await postEphemeral(channelId, userId, botToken, {
    text: "⚖️ Workload review checklist ready.",
    blocks,
  });
}

// ---------------------------------------------------------------------------
// Slack API helpers
// ---------------------------------------------------------------------------

interface PostMessageBody {
  text: string;
  blocks?: unknown[];
}

/**
 * postMessage — calls chat.postMessage to post a visible channel message.
 */
async function postMessage(
  channelId: string,
  botToken: string,
  body: PostMessageBody,
): Promise<SlackAPIResponse> {
  const response = await fetch("https://slack.com/api/chat.postMessage", {
    method: "POST",
    headers: {
      "Content-Type": "application/json; charset=utf-8",
      Authorization: `Bearer ${botToken}`,
    },
    body: JSON.stringify({ channel: channelId, ...body }),
  });

  const data = (await response.json()) as SlackAPIResponse;
  if (!data.ok) {
    console.error(`[action_handlers] chat.postMessage failed: ${data.error}`);
  }
  return data;
}

/**
 * postEphemeral — calls chat.postEphemeral so only the target user sees the message.
 */
async function postEphemeral(
  channelId: string,
  userId: string,
  botToken: string,
  body: PostMessageBody,
): Promise<SlackAPIResponse> {
  const response = await fetch("https://slack.com/api/chat.postEphemeral", {
    method: "POST",
    headers: {
      "Content-Type": "application/json; charset=utf-8",
      Authorization: `Bearer ${botToken}`,
    },
    body: JSON.stringify({ channel: channelId, user: userId, ...body }),
  });

  const data = (await response.json()) as SlackAPIResponse;
  if (!data.ok) {
    console.error(`[action_handlers] chat.postEphemeral failed: ${data.error}`);
  }
  return data;
}

// ---------------------------------------------------------------------------
// Block Kit helpers
// ---------------------------------------------------------------------------

/**
 * buildEmojiButtons
 *
 * Produces an array of plain_text button elements for an Actions block.
 * Each button uses the emoji as its label and posts a unique action_id
 * combining the baseActionId with the value.
 */
function buildEmojiButtons(
  options: Array<{ emoji: string; label: string; value: string }>,
  baseActionId: string,
): unknown[] {
  return options.map(({ emoji, label, value }) => ({
    type: "button",
    text: { type: "plain_text", text: emoji, emoji: true },
    // Accessibility: set the accessible label (screen readers)
    accessibility_label: label,
    action_id: `${baseActionId}_${value}`,
    value,
  }));
}

// ---------------------------------------------------------------------------
// Date helpers
// ---------------------------------------------------------------------------

/**
 * getNextMonday
 *
 * Returns a formatted string for the next Monday from today's date.
 * Used to suggest a concrete no-meeting day date in the draft message.
 */
function getNextMonday(): string {
  const today = new Date();
  const dayOfWeek = today.getDay(); // 0 = Sunday, 1 = Monday, ...
  // Days until next Monday: if today is Monday (1), next Monday is 7 days away
  const daysUntilMonday = dayOfWeek === 1 ? 7 : (8 - dayOfWeek) % 7;
  const nextMonday = new Date(today);
  nextMonday.setDate(today.getDate() + daysUntilMonday);

  return nextMonday.toLocaleDateString("en-US", {
    weekday: "long",
    month: "long",
    day: "numeric",
  });
}
