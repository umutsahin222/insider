-- One row per provider call: the audit trail behind retries (ADR-002).
CREATE TABLE delivery_attempts (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    notification_id uuid NOT NULL REFERENCES notifications (id) ON DELETE CASCADE,
    attempt         integer NOT NULL CHECK (attempt > 0),
    outcome         text NOT NULL CHECK (outcome IN ('success', 'retryable_error', 'permanent_error')),
    status_code     integer,
    error           text,
    duration_ms     integer NOT NULL CHECK (duration_ms >= 0),
    created_at      timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT delivery_attempts_unique_attempt UNIQUE (notification_id, attempt)
);
