-- Coarse last use lets an admin tell a live device from a lost one.
ALTER TABLE device_tokens ADD COLUMN last_used_at timestamptz;
