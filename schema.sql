CREATE TABLE pair_snapshots (
    timestamp TIMESTAMPTZ NOT NULL,     -- Time of the snapshot
    pair_address TEXT NOT NULL,        -- Solana address of the liquidity pool
    base_token_address TEXT NOT NULL,
    base_token_symbol TEXT,
    quote_token_address TEXT NOT NULL,
    quote_token_symbol TEXT,
    price_native NUMERIC,             -- Price of base in terms of quote
    price_usd NUMERIC,
    liquidity_usd NUMERIC,
    volume_m5 NUMERIC,
    volume_h1 NUMERIC,
    volume_h6 NUMERIC,
    volume_h24 NUMERIC,
    price_change_m5 REAL,             -- Using REAL for percentage changes
    price_change_h1 REAL,
    price_change_h6 REAL,
    price_change_h24 REAL,
    txns_m5_buys INTEGER,
    txns_m5_sells INTEGER,
    txns_h1_buys INTEGER,
    txns_h1_sells INTEGER,
    pair_created_at TIMESTAMPTZ,      -- Timestamp when the pair was created

    -- Composite Primary Key ensures uniqueness per pair per timestamp
    PRIMARY KEY (timestamp, pair_address)
);

-- Create an index for efficient time-based queries (Crucial!)
CREATE INDEX idx_pair_snapshots_timestamp ON pair_snapshots (timestamp DESC);
-- Optional index for querying specific pairs over time
CREATE INDEX idx_pair_snapshots_pair_timestamp ON pair_snapshots (pair_address, timestamp DESC);

-- Optional: Hypertable for TimescaleDB later
-- SELECT create_hypertable('pair_snapshots', 'timestamp');