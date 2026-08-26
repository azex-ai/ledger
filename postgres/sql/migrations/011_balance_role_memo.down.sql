ALTER TABLE classifications DROP CONSTRAINT classifications_balance_role_check;
ALTER TABLE classifications ADD CONSTRAINT classifications_balance_role_check
    CHECK (balance_role IN ('', 'available', 'pending', 'locked'));
