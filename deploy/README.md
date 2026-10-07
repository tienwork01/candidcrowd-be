# Production deployment

GitHub Actions builds the API image, pushes both `latest` and the full commit
SHA tag to GHCR, then connects to the VPS over SSH. The VPS never receives or
builds application source code.

## One-time VPS setup

Install Docker Engine with the Compose v2 plugin. The deployment user must be
able to run `docker` without `sudo` and read `/opt/candid-crowd`.

Create the deployment directory and place only this directory's
`docker-compose.yml` there:

```text
/opt/candid-crowd/
|-- docker-compose.yml
`-- .env
```

Create `/opt/candid-crowd/.env` with mode `0600`. Start from the application's
root `.env.example`, use production values, and add:

```dotenv
API_IMAGE=ghcr.io/tienwork01/candidcrowd-be:latest
API_BIND_ADDRESS=127.0.0.1
API_PORT=8080

DATABASE_URL=postgres://<user>:<url-encoded-password>@<external-postgres-host>:5432/<database>?sslmode=require
REDIS_URL=rediss://:<url-encoded-password>@<external-redis-host>:6379/0
```

PostgreSQL and Redis are external services and are not started by this Compose
file. Their network/firewall rules must allow connections from the VPS. Adjust
TLS parameters to match each provider. If a password contains reserved URL
characters, percent-encode it in the connection URL.

Deployment intentionally does not pass `--remove-orphans`, so PostgreSQL or
Redis containers from an older version of this Compose project are not deleted
automatically. Stop and remove them manually only after confirming their data
is no longer needed.

Set the remaining required application values, especially Better Auth, CORS,
and Cloudflare R2 credentials. Keep this file only on the VPS and never commit
it.

Apply database migrations separately before deploying a release that requires
new schema. The application intentionally does not auto-migrate on startup.

## GitHub repository setup

Add these Actions secrets:

- `VPS_HOST`: VPS hostname or IP address reachable on SSH port 22.
- `VPS_USER`: deployment user with Docker access.
- `VPS_SSH_KEY`: private key whose public key is authorized for that user.

The workflow uses the repository-scoped `GITHUB_TOKEN` for GHCR push and for
the deployment-time pull; no long-lived registry password is copied to the
repository. Ensure the GHCR package is linked to this repository so its Actions
token can read it.

Pushes to `master` deploy the immutable commit-SHA image. Compose waits until
the existing `/readyz` endpoint confirms that the API can reach the external
PostgreSQL and Redis services.
