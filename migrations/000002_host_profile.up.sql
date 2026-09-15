ALTER TABLE users ADD COLUMN display_name text NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN email_verified_at timestamptz;
ALTER TABLE users ADD COLUMN last_authenticated_at timestamptz;
CREATE TABLE user_consents (user_id uuid NOT NULL REFERENCES users(id), document_type text NOT NULL, document_version text NOT NULL, accepted_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY (user_id, document_type, document_version));
