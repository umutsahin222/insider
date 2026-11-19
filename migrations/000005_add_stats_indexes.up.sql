-- Indexes for the time-windowed statistics behind GET /v1/stats (ADR-007).
-- On a large live table these would be created CONCURRENTLY, which cannot run inside the
-- migration's transaction; at this project's scale a plain CREATE INDEX is fine.
CREATE INDEX delivery_attempts_created_idx ON delivery_attempts (created_at);
CREATE INDEX notifications_sent_at_idx ON notifications (sent_at) WHERE status = 'sent';
CREATE INDEX notifications_failed_updated_idx ON notifications (updated_at) WHERE status = 'failed';
