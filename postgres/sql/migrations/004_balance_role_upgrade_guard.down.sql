-- Allow '' -> 'available' again regardless of history.
--
-- After this, one UPDATE by the application credential can promote any
-- user-side classification into the spendable bucket, turning whatever
-- balance it already holds into withdrawable funds. Roll back only to
-- unblock.
CREATE OR REPLACE FUNCTION ledger_classifications_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    mutable CONSTANT text[] := ARRAY['display_label', 'balance_role', 'lifecycle', 'is_active'];
BEGIN
    IF NEW.normal_side IS DISTINCT FROM OLD.normal_side THEN
        RAISE EXCEPTION 'ledger: classifications.normal_side is immutable; it determines the sign of every historical rollup for this classification'
            USING ERRCODE = 'check_violation';
    END IF;

    IF NEW.balance_role IS DISTINCT FROM OLD.balance_role AND OLD.balance_role <> '' THEN
        RAISE EXCEPTION 'ledger: classifications.balance_role is already set to %; only the '''' -> <role> upgrade is allowed', OLD.balance_role
            USING ERRCODE = 'check_violation';
    END IF;

    IF (to_jsonb(OLD) - mutable) IS DISTINCT FROM (to_jsonb(NEW) - mutable) THEN
        RAISE EXCEPTION 'ledger: UPDATE on classifications may only change %, and this statement changed something else',
            array_to_string(mutable, ', ')
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NEW;
END;
$$;
