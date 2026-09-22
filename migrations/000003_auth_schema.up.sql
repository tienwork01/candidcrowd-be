CREATE SCHEMA IF NOT EXISTS auth;
DO $$
BEGIN
  IF EXISTS (SELECT FROM pg_roles WHERE rolname = 'candidcrowd') THEN
    GRANT USAGE, CREATE ON SCHEMA auth TO candidcrowd;
  END IF;
END
$$;
