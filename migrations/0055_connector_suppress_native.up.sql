-- 0055: per-connector native-notification suppression (Spec 004, manyforge-a7j.8). When true,
-- a reply on a ticket linked to this connector skips the native ticket.replied email — the
-- external system (Jira/Zendesk) is the single notification channel. Default false preserves
-- the additive both-notify behavior shipped in US4.
ALTER TABLE connector ADD COLUMN suppress_native_notifications boolean NOT NULL DEFAULT false;
