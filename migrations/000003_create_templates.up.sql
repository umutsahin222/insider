CREATE TABLE templates (
    id         uuid PRIMARY KEY,
    name       text NOT NULL UNIQUE,
    channel    notification_channel NOT NULL,
    body       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE notifications
    ADD COLUMN template_id uuid REFERENCES templates (id) ON DELETE SET NULL;
