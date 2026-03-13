CREATE TABLE user_settings (
  username text PRIMARY KEY,
  anthropic_api_key text,
  preferred_model text DEFAULT 'claude-haiku-4-5-20251001',
  created_at timestamptz DEFAULT now(),
  updated_at timestamptz DEFAULT now()
);
