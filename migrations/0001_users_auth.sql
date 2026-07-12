-- +goose Up
CREATE TABLE users
(
    id                 uuid PRIMARY KEY,
    email              varchar UNIQUE,
    phone              varchar UNIQUE,
    full_name          varchar     NOT NULL DEFAULT '',
    role               varchar     NOT NULL DEFAULT 'user',
    avatar_url         varchar,
    preferred_language varchar     NOT NULL DEFAULT 'ru',
    city               varchar,
    is_active          boolean     NOT NULL DEFAULT true,
    email_verified_at  timestamptz,
    phone_verified_at  timestamptz,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE user_credentials
(
    user_id       uuid PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    password_hash varchar NOT NULL
);

CREATE TABLE otp_codes
(
    id         uuid PRIMARY KEY,
    phone      varchar     NOT NULL,
    code_hash  varchar     NOT NULL,
    channel    varchar,
    attempts   int         NOT NULL DEFAULT 0,
    used_at    timestamptz,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_otp_codes_phone_created ON otp_codes (phone, created_at DESC);
CREATE INDEX idx_otp_codes_expires ON otp_codes (expires_at);

CREATE TABLE refresh_tokens
(
    id         uuid PRIMARY KEY,
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash varchar     NOT NULL UNIQUE,
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    user_agent varchar,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_refresh_tokens_user ON refresh_tokens (user_id);

-- +goose Down
DROP TABLE refresh_tokens;
DROP TABLE otp_codes;
DROP TABLE user_credentials;
DROP TABLE users;
