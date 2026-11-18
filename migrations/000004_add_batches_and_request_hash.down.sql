ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_batch_fk;
DROP TABLE IF EXISTS batches;
ALTER TABLE notifications DROP COLUMN IF EXISTS request_hash;
