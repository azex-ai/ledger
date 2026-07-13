-- Crypto deposit + sweep registry (docs/plans/2026-07-11-crypto-deposit-sweep-design.md
-- §2/§3): the address<->holder registry backing CREATE2 deposit addresses,
-- plus the watcher's per-chain scan-progress cursor. Both tables are new
-- (this feature does not exist yet), so uid is NOT NULL from the start --
-- no backfill dance needed (contrast migration 031, which had to retrofit
-- uid onto pre-existing rows).

-- deposit_addresses: one CREATE2-derived custody address per account holder.
-- factory/init_hash are recorded per row (not just read from config) so a
-- future factory redeploy or init-hash change is auditable per address
-- instead of only visible in current config.
CREATE TABLE IF NOT EXISTS deposit_addresses (
    id             BIGSERIAL PRIMARY KEY,
    uid            UUID NOT NULL,
    account_holder BIGINT NOT NULL CHECK (account_holder > 0),
    address        TEXT NOT NULL,
    factory        TEXT NOT NULL,
    init_hash      TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_deposit_addresses_uid ON deposit_addresses (uid);
-- One address per holder (design doc §0: "salt=holder" locks a 1:1 mapping;
-- no address rotation / multi-address-per-holder this period).
CREATE UNIQUE INDEX IF NOT EXISTS uq_deposit_addresses_account_holder ON deposit_addresses (account_holder);
-- Reverse lookup (watcher: observed `to` address -> holder). Addresses are
-- always written in the canonical EIP-55 checksum casing produced by
-- DeriveDepositAddress, so a plain (case-sensitive) unique index is
-- sufficient as long as every write and read path normalizes through that
-- same derivation/casing -- store adapters must not bypass it.
CREATE UNIQUE INDEX IF NOT EXISTS uq_deposit_addresses_address ON deposit_addresses (address);

-- chain_cursors: watcher's log-scan progress per chain, so a restart resumes
-- instead of rescanning from genesis or skipping unseen blocks.
CREATE TABLE IF NOT EXISTS chain_cursors (
    chain_id           BIGINT PRIMARY KEY,
    last_scanned_block BIGINT NOT NULL DEFAULT 0,
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
