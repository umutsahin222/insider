DROP TRIGGER IF EXISTS notifications_ready_after_update ON notifications;
DROP TRIGGER IF EXISTS notifications_ready_after_insert ON notifications;
DROP FUNCTION IF EXISTS notify_ready_work();
