# Technical Stack & Coding Standards

## Backend: Go (Golang)
- Use Go 1.21+.
- Use the standard library `net/http` for the webhook server.
- Use `crypto/hmac` and `crypto/sha256` for the privacy hashing.
- Use `github.com/mattn/go-sqlite3` for database persistence.
- Prioritize memory safety. Ensure the temporary hash map mapping anonymous IDs to message counts is explicitly cleared/garbage collected after the integer slice is extracted.

## Frontend: Slack Agent (TypeScript / Deno)
- The Slack Agent runs on Deno (standard for Slack next-gen platform).
- Use the `@slack/deno-slack-sdk` for manifest and function definitions.
- **LLM Integration:** Configure the agent to use the Anthropic API, specifically targeting the `claude-3-5-sonnet-20240620` model.
- **UI:** The agent must output valid Slack Block Kit JSON arrays, specifically utilizing the new Agent Kit components (Cards, Alerts, Buttons) rather than raw Markdown.

## Privacy Rule (Critical)
The Go backend must NEVER write the raw `user_id` or the temporary hashed `anon_id` to the SQLite database or print them to standard out. Only mathematical scalars (floats/ints) representing the channel's aggregate shape may be persisted.
