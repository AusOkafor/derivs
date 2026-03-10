-- Alert history: logs every alert that fires (regardless of subscriber dedup)
CREATE TABLE alert_history (
    id UUID DEFAULT gen_random_uuid() PRIMARY KEY,
    symbol TEXT NOT NULL,
    alert_id TEXT NOT NULL,
    message TEXT NOT NULL,
    severity TEXT NOT NULL,
    triggered_at TIMESTAMPTZ DEFAULT now()
);

CREATE INDEX alert_history_symbol_idx ON alert_history(symbol, triggered_at DESC);
