DROP TRIGGER IF EXISTS notifications_status_events_after_update ON notifications;
DROP TRIGGER IF EXISTS notifications_status_events_after_insert ON notifications;
DROP FUNCTION IF EXISTS record_changed_status_events();
DROP FUNCTION IF EXISTS record_created_status_events();
DROP TABLE IF EXISTS status_event_outbox;
