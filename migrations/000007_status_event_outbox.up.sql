-- Transactional outbox for status events (ADR-012). Every status change of a notification is
-- recorded here by the same statement that makes it, so an event exists if and only if the
-- change committed. A relay in the worker moves the rows to a Redis stream, oldest first, and
-- deletes them.
CREATE TABLE status_event_outbox (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    notification_id uuid NOT NULL,
    batch_id        uuid,
    status          notification_status NOT NULL,
    attempts        integer NOT NULL,
    error           text,
    occurred_at     timestamptz NOT NULL
);

-- An event mirrors the row after the change: status, attempt number and last_error.
CREATE FUNCTION record_created_status_events() RETURNS trigger AS $$
BEGIN
    INSERT INTO status_event_outbox (notification_id, batch_id, status, attempts, error, occurred_at)
    SELECT n.id, n.batch_id, n.status, n.attempts, n.last_error, n.updated_at
    FROM created_rows AS n
    ORDER BY n.id;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE FUNCTION record_changed_status_events() RETURNS trigger AS $$
BEGIN
    INSERT INTO status_event_outbox (notification_id, batch_id, status, attempts, error, occurred_at)
    SELECT n.id, n.batch_id, n.status, n.attempts, n.last_error, n.updated_at
    FROM changed_rows AS n
    JOIN previous_rows AS o ON o.id = n.id
    WHERE n.status IS DISTINCT FROM o.status
    ORDER BY n.id;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER notifications_status_events_after_insert
    AFTER INSERT ON notifications
    REFERENCING NEW TABLE AS created_rows
    FOR EACH STATEMENT EXECUTE FUNCTION record_created_status_events();

CREATE TRIGGER notifications_status_events_after_update
    AFTER UPDATE ON notifications
    REFERENCING OLD TABLE AS previous_rows NEW TABLE AS changed_rows
    FOR EACH STATEMENT EXECUTE FUNCTION record_changed_status_events();
