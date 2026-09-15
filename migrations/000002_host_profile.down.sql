DROP TABLE IF EXISTS user_consents;
ALTER TABLE users DROP COLUMN IF EXISTS last_authenticated_at;
ALTER TABLE users DROP COLUMN IF EXISTS email_verified_at;
ALTER TABLE users DROP COLUMN IF EXISTS display_name;
