-- Account Policy (design doc §4, 2026-07-02): optional per-dimension override
-- rows expressing freeze/close + balance-floor controls on the otherwise
-- implicit (account_holder, currency_id, classification_id) account
-- dimension. No policy row for a dimension = today's behaviour (active,
-- unconstrained) — this is additive, not a breaking change to existing data.
CREATE TABLE account_policies (
    id                  BIGSERIAL PRIMARY KEY,
    account_holder      BIGINT NOT NULL CHECK (account_holder <> 0),
    currency_id         BIGINT NOT NULL DEFAULT 0,  -- 0 = all currencies for this holder
    classification_id   BIGINT NOT NULL DEFAULT 0,  -- 0 = all classifications for this holder/currency
    status              TEXT NOT NULL DEFAULT 'active'
                        CHECK (status IN ('active', 'frozen', 'closed')),
    -- Balance floor: 0 = no overdraft; negative = overdraft/credit limit
    -- (|min_balance| is the limit); positive = dust floor (financial.md's
    -- $0.10 rule can be declared here). Only enforced when
    -- enforce_min_balance is true.
    min_balance         NUMERIC(30,18) NOT NULL DEFAULT 0,
    enforce_min_balance BOOLEAN NOT NULL DEFAULT false,
    note                TEXT NOT NULL DEFAULT '',
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (account_holder, currency_id, classification_id)
);

CREATE INDEX idx_account_policies_holder ON account_policies (account_holder);

-- Policy changes are operational config (not funds) so the row above is
-- UPDATEd in place, but every change is appended here for audit trail —
-- mirrors the append-only discipline applied to journals without forcing
-- the same immutability on non-financial config rows.
CREATE TABLE account_policy_changes (
    id         BIGSERIAL PRIMARY KEY,
    policy_id  BIGINT NOT NULL REFERENCES account_policies(id),
    old_state  JSONB NOT NULL,
    new_state  JSONB NOT NULL,
    actor_id   BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_account_policy_changes_policy ON account_policy_changes (policy_id, created_at DESC);
