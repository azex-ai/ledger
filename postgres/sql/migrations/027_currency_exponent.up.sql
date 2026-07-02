-- Currency exponent declares the maximum number of decimal places an entry
-- amount may carry for that currency (JPY=0, USD=2, USDT=6, wei=18).
-- NUMERIC(30,18) is storage precision, not business precision: without this
-- column, a 0.001 JPY entry is perfectly legal today. The write path (see
-- core.ErrPrecisionExceeded) rejects any amount whose scale exceeds the
-- currency's exponent — it never silently rounds or truncates.
--
-- Existing rows default to 18 (the loosest possible setting, i.e. today's
-- de-facto behavior) so no historical data is invalidated. Deployments that
-- want tighter enforcement update existing rows explicitly.
ALTER TABLE currencies
    ADD COLUMN exponent SMALLINT NOT NULL DEFAULT 18
    CHECK (exponent >= 0 AND exponent <= 18);
