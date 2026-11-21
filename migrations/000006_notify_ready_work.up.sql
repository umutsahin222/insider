-- Wake idle workers when notifications become claimable right away (ADR-011).
-- One NOTIFY per channel and statement, sent on commit, so a worker never wakes before the
-- rows are visible; a 1000-item batch wakes each channel once. Scheduled rows and retries
-- that back off are not announced: workers' polling picks those up when they fall due.
CREATE FUNCTION notify_ready_work() RETURNS trigger AS $$
BEGIN
    PERFORM pg_notify('notifications_ready', ready.channel)
    FROM (SELECT DISTINCT channel::text AS channel
          FROM changed_rows
          WHERE status = 'pending' AND next_attempt_at <= now()) AS ready;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

-- Created notifications (single and batch).
CREATE TRIGGER notifications_ready_after_insert
    AFTER INSERT ON notifications
    REFERENCING NEW TABLE AS changed_rows
    FOR EACH STATEMENT EXECUTE FUNCTION notify_ready_work();

-- Rows put back to pending and due now: manual retry, release, reaped lease.
CREATE TRIGGER notifications_ready_after_update
    AFTER UPDATE ON notifications
    REFERENCING NEW TABLE AS changed_rows
    FOR EACH STATEMENT EXECUTE FUNCTION notify_ready_work();
