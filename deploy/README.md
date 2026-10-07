# Production deployment

GitHub Actions builds the API image, pushes both `latest` and the full commit
SHA tag to GHCR, then connects to the VPS over SSH. The VPS never receives or
builds application source code.

## One-time VPS setup

Install Docker Engine with the Compose v2 plugin. The deployment user must be
able to run `docker` without `sudo` and read `/opt/candid-crowd`.

Run these commands once on the VPS as the deployment user (use `sudo` only for
directory ownership):

```bash
sudo mkdir -p /opt/candid-crowd
sudo chown "$USER":"$USER" /opt/candid-crowd
docker version
docker compose version
curl --version

read -rsp "GHCR PAT (read:packages): " GHCR_PAT
printf '\n'
printf '%s' "$GHCR_PAT" | docker login ghcr.io --username tienwork01 --password-stdin
unset GHCR_PAT
```

For a private organization package, authorize the PAT for the organization if
its SSO policy requires it.

Create the deployment directory and place only this directory's
`docker-compose.yml` there:

```text
/opt/candid-crowd/
|-- docker-compose.yml
`-- .env
```

Create `/opt/candid-crowd/.env` with mode `0600`. Start from the application's
root `.env.example`, use production values, and add the optional deployment
overrides if needed:

```dotenv
# The workflow overrides this with the immutable commit SHA during deployment.
API_IMAGE=ghcr.io/tienwork01/candidcrowd-be:latest
API_BIND_ADDRESS=127.0.0.1
API_PORT=8080

DATABASE_URL=postgres://<user>:<url-encoded-password>@<external-postgres-host>:5432/<database>?sslmode=require
REDIS_URL=rediss://:<url-encoded-password>@<external-redis-host>:6379/0
```

```bash
chmod 600 /opt/candid-crowd/.env
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

GitHub Actions uses the repository-scoped `GITHUB_TOKEN` only to push the image
to GHCR. Log the VPS in to GHCR once with a classic PAT that has
`read:packages`; the workflow reuses the credential stored by Docker on the
VPS and never sends a registry token over SSH.

Pushes to `master` deploy the immutable commit-SHA image. Compose waits until
the API container is running, then `curl` on the VPS polls the existing
`/readyz` endpoint until the API can reach the external PostgreSQL and Redis
services. A successful deployment prunes dangling Docker images.
