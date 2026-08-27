-- Durable, confirm-time record of FEE_FUND fee-wallet spend, written by the
-- sweeper in the same transaction that confirms a funding leg (sweeps/broadcast
-- Confirm). It backs the fee-wallet spend cap: the cap must not depend on the
-- asynchronous deposit scanner's FEE_FUNDING ledger rows, which lag behind a
-- funding leg's confirmation (the reservation slot frees at confirm, so the
-- next leg could start before the scanner recorded the prior spend). Recording
-- the spend here, atomically with the confirm, closes that race. Idempotent on
-- (chain_id, tx_hash): one funding tx is one spend.
CREATE TABLE fee_funding_spends (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    chain_id TEXT NOT NULL,
    tx_hash TEXT NOT NULL,
    fee_wallet_address TEXT NOT NULL,
    amount_base_units TEXT NOT NULL CHECK (amount_base_units <> '' AND amount_base_units NOT GLOB '*[^0-9]*'),
    block_height INTEGER NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE (chain_id, tx_hash)
);

CREATE INDEX ix_fee_funding_spends_window
    ON fee_funding_spends (chain_id, fee_wallet_address, block_height);
