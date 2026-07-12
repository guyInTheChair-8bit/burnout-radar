-- BurnoutRadar MCP Daemon — channel_metrics schema
-- ============================================================
-- ZKA GUARANTEE: This table stores ONLY final mathematical scalars.
-- No user IDs, no message text, no raw counts are ever inserted here.
-- The channel_id is a Slack channel identifier (e.g. C012AB3CD) which
-- identifies a *team channel*, not an individual — acceptable under ZKA.
-- ============================================================

CREATE TABLE IF NOT EXISTS channel_metrics (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    date            TEXT    NOT NULL,           -- Format: YYYY-MM-DD (UTC)
    channel_id      TEXT    NOT NULL,           -- Slack channel ID (not a person)
    gini_coeff      REAL,                       -- Gini coefficient [0,1]
    pareto_share    REAL,                       -- Top-20% message share [0,100]
    z_score         REAL,                       -- Standard deviations from mean
    dm_share_pct    REAL,                       -- Percentage of messages that are DMs
    avg_word_count  REAL,                       -- Mean words per message (aggregate only)
    UNIQUE(date, channel_id)                    -- One row per channel per day
);

-- ============================================================
-- Operational queries (embedded as comments for reference):
-- ============================================================

-- INSERT OR REPLACE (upsert on unique constraint):
-- INSERT OR REPLACE INTO channel_metrics
--     (date, channel_id, gini_coeff, pareto_share, z_score, dm_share_pct, avg_word_count)
-- VALUES (?, ?, ?, ?, ?, ?, ?);

-- SELECT metrics for a given channel on a given date:
-- SELECT id, date, channel_id, gini_coeff, pareto_share, z_score, dm_share_pct, avg_word_count
-- FROM channel_metrics
-- WHERE date = ? AND channel_id = ?;

-- SELECT historical mean volume for z-score baseline:
-- SELECT AVG(total_messages) FROM channel_metrics WHERE channel_id = ?;
