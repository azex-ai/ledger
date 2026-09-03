-- Reverses 028. Dropping the columns loses every stored signature, which is
-- the honest consequence: a rollback to the code that predates 028 does not
-- read them, and its gated Reserve is the I-49 conservative rule that
-- credits no discharge at all -- strictly safer, never more permissive. So
-- this down is safe in the direction that matters (it cannot let money out
-- that the up would have held), and irreversible in the direction that does
-- not (a re-applied 028 starts with unsigned claims, which are treated as
-- untrusted, i.e. as full holds, until new ones are written).

ALTER TABLE reservation_settlement_legs
    DROP COLUMN IF EXISTS auth_key_id,
    DROP COLUMN IF EXISTS auth_signature,
    DROP COLUMN IF EXISTS auth_digest;

ALTER TABLE reservation_operation_receipts
    DROP COLUMN IF EXISTS auth_key_id,
    DROP COLUMN IF EXISTS auth_signature,
    DROP COLUMN IF EXISTS auth_digest;
