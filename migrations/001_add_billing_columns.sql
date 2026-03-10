-- Add Stripe billing columns to subscribers table
ALTER TABLE subscribers 
ADD COLUMN IF NOT EXISTS tier TEXT NOT NULL DEFAULT 'free',
ADD COLUMN IF NOT EXISTS stripe_customer_id TEXT,
ADD COLUMN IF NOT EXISTS stripe_subscription_id TEXT,
ADD COLUMN IF NOT EXISTS subscription_status TEXT DEFAULT 'inactive',
ADD COLUMN IF NOT EXISTS pro_since TIMESTAMPTZ;
