-- Restores migration 018's direct-INSERT grant on anchor_observations and
-- drops the SECURITY DEFINER writer.
--
-- ⚠️ Must run while the function and anchor_observations are owned by the
-- credential issuing this file, or from a superuser connection -- the same
-- fence 019's and 020's down scripts carry.

DROP FUNCTION IF EXISTS ledger_record_anchor_observation(uuid, bigint, bytea);

GRANT INSERT ON public.anchor_observations TO ledger_app;
GRANT USAGE, SELECT ON SEQUENCE public.anchor_observations_id_seq TO ledger_app;
