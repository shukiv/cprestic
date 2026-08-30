-- Human accounts for the web UI, and one-time agent enrolment tokens.
--
-- The UI holds every destination credential in the fleet, so nothing here
-- stores a usable secret: passwords are argon2id digests, session cookies
-- and enrolment tokens are stored only as hashes.

BEGIN;

CREATE TABLE users (
    id            uuid PRIMARY KEY,
    email         text NOT NULL,
    -- Encoded argon2id digest, carrying its own parameters so they can be
    -- raised later without invalidating existing passwords.
    password_hash text NOT NULL,
    display_name  text,
    disabled      boolean NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_login_at timestamptz
);

-- Addresses differ only by case as far as people are concerned.
CREATE UNIQUE INDEX users_email_key ON users (lower(email));

CREATE TABLE sessions (
    -- The SHA-256 of the cookie value, never the value itself: a copy of
    -- this table must not yield a usable session.
    id           text PRIMARY KEY,
    user_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    csrf_token   text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    ip           text,
    user_agent   text
);

CREATE INDEX sessions_user_idx ON sessions (user_id);
CREATE INDEX sessions_expiry_idx ON sessions (expires_at);

CREATE TABLE enrolment_tokens (
    id         uuid PRIMARY KEY,
    -- SHA-256 of the token. The plaintext exists only in the response that
    -- minted it.
    token_hash bytea NOT NULL UNIQUE,
    -- The server row an operator already created. Redeeming fills in its
    -- certificate fingerprint; it never creates a server.
    server_id  uuid NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
    hostname   text NOT NULL,
    created_by uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    used_at    timestamptz,
    used_ip    text
);

CREATE INDEX enrolment_tokens_server_idx ON enrolment_tokens (server_id);

COMMIT;
