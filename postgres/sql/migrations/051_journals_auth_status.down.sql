-- 051_journals_auth_status.down.sql
ALTER TABLE journals DROP COLUMN IF EXISTS auth_status;
