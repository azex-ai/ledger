-- Balance role: a semantic liquidity tag on classifications so the library
-- can expose a consumer-facing balance breakdown (available / pending /
-- locked) without hardcoding preset classification codes in core.
--
--   ''          — not part of the holder's spendable-money view (fee_expense,
--                 suspense, custodial, revenue/expense classifications).
--   'available' — immediately spendable funds (e.g. main_wallet).
--   'pending'   — inbound funds awaiting confirmation (e.g. pending deposits).
--   'locked'    — journal-locked funds (e.g. withdrawal in flight).
--
-- Reservation holds are NOT a role: they live in the reservations table and
-- are layered on top of the role sums at read time (available -= held,
-- locked += held).
ALTER TABLE classifications
    ADD COLUMN IF NOT EXISTS balance_role TEXT NOT NULL DEFAULT ''
    CHECK (balance_role IN ('', 'available', 'pending', 'locked'));
