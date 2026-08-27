-- Sovren Exchange Integration Kit — PostgreSQL schema, migration 0001.
-- Dialect notes: timestamps are RFC3339 UTC TEXT (kept identical to the
-- SQLite dialect so repo code is shared); amounts are integer-string TEXT
-- (arbitrary precision — never summed in SQL). Every uniqueness rule from
-- data-model.md is a real constraint here (R7).

CREATE TABLE ledger_entries (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    chain_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('TX','BLOCK_EVENT')),
    tx_hash TEXT,
    message_index BIGINT,
    op_index BIGINT,
    block_height BIGINT NOT NULL,
    event_index BIGINT,
    direction TEXT NOT NULL CHECK (direction IN ('IN','OUT')),
    address TEXT NOT NULL,
    counterparty_set TEXT NOT NULL,
    amount_base_units TEXT NOT NULL CHECK (amount_base_units ~ '^[0-9]+$'),
    denom TEXT NOT NULL,
    tx_code BIGINT,
    classification TEXT NOT NULL CHECK (classification IN ('EXTERNAL_DEPOSIT','INTERNAL_TRANSFER','FEE_FUNDING','SWEEP','WITHDRAWAL','FEE_DEDUCTION','UNATTRIBUTED_REVIEW')),
    created_at TEXT NOT NULL,
    CHECK (
        (kind = 'TX' AND tx_hash IS NOT NULL AND message_index IS NOT NULL AND op_index IS NOT NULL)
        OR (kind = 'BLOCK_EVENT' AND tx_hash IS NULL AND event_index IS NOT NULL)
    )
);

CREATE UNIQUE INDEX ux_ledger_tx
    ON ledger_entries (chain_id, tx_hash, message_index, op_index)
    WHERE kind = 'TX';

CREATE UNIQUE INDEX ux_ledger_block_event
    ON ledger_entries (chain_id, block_height, event_index)
    WHERE kind = 'BLOCK_EVENT';

CREATE INDEX ix_ledger_address ON ledger_entries (chain_id, address, block_height);

CREATE TABLE fee_outflows (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    chain_id TEXT NOT NULL,
    tx_hash TEXT NOT NULL,
    payer_address TEXT NOT NULL,
    fee_base_units TEXT NOT NULL CHECK (fee_base_units ~ '^[0-9]+$'),
    tx_code BIGINT NOT NULL,
    block_height BIGINT NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE (chain_id, tx_hash)
);

CREATE INDEX ix_fee_outflows_payer ON fee_outflows (chain_id, payer_address, block_height);

CREATE TABLE deposits (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    chain_id TEXT NOT NULL,
    tx_hash TEXT NOT NULL,
    message_index BIGINT NOT NULL,
    coin_index BIGINT NOT NULL,
    recipient_address TEXT NOT NULL,
    block_height BIGINT NOT NULL,
    block_timestamp TEXT NOT NULL,
    sender_address TEXT,
    denom TEXT NOT NULL CHECK (denom = 'usovr'),
    amount_base_units TEXT NOT NULL CHECK (amount_base_units ~ '^[0-9]+$' AND amount_base_units <> '0'),
    memo TEXT NOT NULL DEFAULT '',
    tx_code BIGINT NOT NULL,
    tx_log TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL CHECK (status IN ('DISCOVERED','VALIDATED','AWAITING_CONFIRMATIONS','CREDITABLE','CREDITED','SWEEP_PENDING','SWEPT','REJECTED','REVIEW_REQUIRED','ORPHANED','DUPLICATE','BELOW_MINIMUM','SUSPENDED')),
    prior_status TEXT,
    credited_at TEXT,
    sweep_tx_hash TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    CHECK ((status = 'SUSPENDED') = (prior_status IS NOT NULL)),
    UNIQUE (chain_id, tx_hash, message_index, coin_index, recipient_address)
);

CREATE INDEX ix_deposits_status ON deposits (chain_id, status);

CREATE TABLE scanner_checkpoints (
    chain_id TEXT PRIMARY KEY,
    last_fully_processed_height BIGINT NOT NULL,
    last_observed_block_hash TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE withdrawals (
    withdrawal_id TEXT PRIMARY KEY,
    idempotency_key TEXT NOT NULL,
    chain_id TEXT NOT NULL,
    source_address TEXT NOT NULL,
    destination_address TEXT NOT NULL,
    denom TEXT NOT NULL CHECK (denom = 'usovr'),
    amount_base_units TEXT NOT NULL CHECK (amount_base_units ~ '^[0-9]+$' AND amount_base_units <> '0'),
    memo TEXT NOT NULL DEFAULT '',
    account_number BIGINT,
    sequence BIGINT,
    gas_wanted BIGINT,
    gas_limit BIGINT,
    fee_amount_base_units TEXT CHECK (fee_amount_base_units IS NULL OR fee_amount_base_units ~ '^[0-9]+$'),
    sign_mode TEXT NOT NULL DEFAULT 'SIGN_MODE_DIRECT' CHECK (sign_mode = 'SIGN_MODE_DIRECT'),
    signed_tx_bytes BYTEA,
    tx_hash TEXT,
    block_height BIGINT,
    tx_code BIGINT,
    raw_log TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL CHECK (status IN ('REQUESTED','ADDRESS_VALIDATED','COMPLIANCE_APPROVED','FUNDS_RESERVED','SEQUENCE_RESERVED','TRANSACTION_BUILT','TRANSACTION_SIMULATED','SIGNED','BROADCAST','INCLUDED','CONFIRMED','FAILED','CANCELLED','REVIEW_REQUIRED')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (idempotency_key)
);

CREATE INDEX ix_withdrawals_status ON withdrawals (chain_id, status);

CREATE TABLE sequence_reservations (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    chain_id TEXT NOT NULL,
    source_address TEXT NOT NULL,
    account_number BIGINT NOT NULL,
    sequence BIGINT NOT NULL,
    work_kind TEXT NOT NULL CHECK (work_kind IN ('WITHDRAWAL','SWEEP')),
    work_id TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('RESERVED','SIGNED','BROADCAST','CONSUMED','RELEASED','RECONCILIATION_REQUIRED')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (chain_id, source_address, sequence),
    UNIQUE (work_kind, work_id)
);

CREATE TABLE sweep_jobs (
    sweep_id TEXT PRIMARY KEY,
    idempotency_key TEXT NOT NULL,
    chain_id TEXT NOT NULL,
    source_address TEXT NOT NULL,
    hot_wallet_address TEXT NOT NULL,
    strategy TEXT NOT NULL CHECK (strategy IN ('FEE_RESERVE','FEE_FUND','THRESHOLD_ONLY','CUSTODY_ABSTRACTED')),
    amount_base_units TEXT NOT NULL CHECK (amount_base_units ~ '^[0-9]+$'),
    fee_reserve_base_units TEXT NOT NULL CHECK (fee_reserve_base_units ~ '^[0-9]+$'),
    deposit_ids TEXT NOT NULL DEFAULT '[]',
    signed_tx_bytes BYTEA,
    tx_hash TEXT,
    tx_code BIGINT,
    status TEXT NOT NULL CHECK (status IN ('PENDING','BUILT','SIGNED','BROADCAST','CONFIRMED','DEFERRED','FAILED','CANCELLED')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (idempotency_key)
);

-- At most one non-terminal sweep per (chain_id, source_address) — data
-- model §7 guarantee (1); terminal = CONFIRMED / FAILED / CANCELLED.
CREATE UNIQUE INDEX ux_sweep_nonterminal
    ON sweep_jobs (chain_id, source_address)
    WHERE status NOT IN ('CONFIRMED','FAILED','CANCELLED');

CREATE TABLE watched_addresses (
    chain_id TEXT NOT NULL,
    address TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('CUSTOMER_DEPOSIT','HOT_WALLET','COLD_WALLET','FEE_WALLET','OMNIBUS')),
    customer_ref TEXT NOT NULL DEFAULT '',
    memo_required BOOLEAN NOT NULL DEFAULT FALSE,
    active BOOLEAN NOT NULL DEFAULT TRUE,
    PRIMARY KEY (chain_id, address)
);

CREATE TABLE operational_controls (
    chain_id TEXT PRIMARY KEY,
    credit_paused BOOLEAN NOT NULL DEFAULT FALSE,
    signing_paused BOOLEAN NOT NULL DEFAULT FALSE,
    broadcast_paused BOOLEAN NOT NULL DEFAULT FALSE,
    sweep_paused BOOLEAN NOT NULL DEFAULT FALSE,
    scan_without_credit BOOLEAN NOT NULL DEFAULT FALSE,
    resume_from_height BIGINT,
    updated_at TEXT NOT NULL
);

CREATE TABLE controls_audit (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    chain_id TEXT NOT NULL,
    field TEXT NOT NULL,
    old_value TEXT NOT NULL,
    new_value TEXT NOT NULL,
    actor TEXT NOT NULL,
    reason TEXT NOT NULL,
    occurred_at TEXT NOT NULL
);

CREATE INDEX ix_controls_audit_chain ON controls_audit (chain_id, occurred_at);

CREATE TABLE chain_review_conditions (
    condition_id TEXT PRIMARY KEY,
    chain_id TEXT NOT NULL,
    trigger_kind TEXT NOT NULL CHECK (trigger_kind IN ('BLOCK_HASH_MISMATCH','QUERY_RESULT_MISMATCH','HEIGHT_DIVERGENCE','CHAIN_HALT','WRONG_CHAIN_ID','UPGRADE_WINDOW')),
    node_a_observation TEXT NOT NULL,
    node_b_observation TEXT NOT NULL,
    opened_at TEXT NOT NULL,
    resolved_at TEXT,
    resolution TEXT NOT NULL DEFAULT ''
);

CREATE INDEX ix_chain_review_open ON chain_review_conditions (chain_id, resolved_at);

CREATE TABLE review_items (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    chain_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('DEPOSIT','WITHDRAWAL','LEDGER_ENTRY')),
    ref_id TEXT NOT NULL,
    reason TEXT NOT NULL,
    opened_at TEXT NOT NULL,
    resolved_at TEXT,
    resolution TEXT NOT NULL DEFAULT ''
);

CREATE INDEX ix_review_items_open ON review_items (chain_id, resolved_at);

CREATE TABLE reconciliation_reports (
    report_id TEXT PRIMARY KEY,
    chain_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('TX_NEAR_REAL_TIME','WALLET_HOURLY','ADDRESS_DAILY','MANUAL')),
    period_start TEXT NOT NULL,
    period_end TEXT NOT NULL,
    generated_at TEXT NOT NULL,
    discrepancy_count BIGINT NOT NULL
);

CREATE TABLE reconciliation_entries (
    report_id TEXT NOT NULL REFERENCES reconciliation_reports (report_id),
    entry_index BIGINT NOT NULL,
    address TEXT NOT NULL,
    expected_base_units TEXT NOT NULL,
    observed_base_units TEXT NOT NULL,
    difference TEXT NOT NULL,
    earliest_suspected_height BIGINT NOT NULL,
    related_tx_hashes TEXT NOT NULL DEFAULT '[]',
    recommended_rescan_height BIGINT NOT NULL,
    PRIMARY KEY (report_id, entry_index)
);

CREATE TABLE outbox (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    chain_id TEXT NOT NULL,
    topic TEXT NOT NULL,
    dedup_key TEXT,
    payload BYTEA NOT NULL,
    created_at TEXT NOT NULL,
    dispatched_at TEXT,
    UNIQUE (dedup_key)
);

CREATE INDEX ix_outbox_pending ON outbox (dispatched_at, id);

-- Per-account serialization row: reservation paths take
-- SELECT ... FOR UPDATE on this row, creating it on demand (data model §6).
CREATE TABLE chain_account_locks (
    chain_id TEXT NOT NULL,
    source_address TEXT NOT NULL,
    PRIMARY KEY (chain_id, source_address)
);
