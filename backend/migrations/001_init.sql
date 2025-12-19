CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE IF NOT EXISTS users (
  id uuid PRIMARY KEY,
  email text UNIQUE
);

CREATE TABLE IF NOT EXISTS links (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id uuid NOT NULL REFERENCES users(id),
  code text NOT NULL UNIQUE,
  original_url text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_links_user_id_created_at
  ON links(user_id, created_at DESC);

CREATE TABLE IF NOT EXISTS clicks (
  id bigserial PRIMARY KEY,
  link_id uuid NOT NULL REFERENCES links(id),
  ts timestamptz NOT NULL DEFAULT now(),
  ip inet,
  ua text,
  referer text
);

CREATE INDEX IF NOT EXISTS idx_clicks_link_id_ts
  ON clicks(link_id, ts);

-- Seed demo user (MVP auth)
INSERT INTO users (id, email)
VALUES ('00000000-0000-0000-0000-000000000001', 'demo@example.com')
ON CONFLICT DO NOTHING;
