-- Reverses 032: drops the deposit-identity unique index.
--
-- Going down re-opens N-1: the honest recheck job will again credit every
-- booking that describes an on-chain log, however many of them there are,
-- because the application-level check (service.Onchain's
-- corroborateBeforeConfirm) is the other half of the fence and not a
-- substitute for it -- it reads the same table this index constrains, so a
-- concurrent pair of inserts is exactly what the index is there to settle.
-- That is what a down script is for (getting back to the previous release's
-- behaviour, defects included), but it is worth writing down so nobody reads
-- this file as an alternative implementation.

DROP INDEX IF EXISTS uq_bookings_deposit_identity;
