# Lighthouse

Lighthouse keeps the projects on a personal server up to date: tag a release on GitHub, and it builds, checks and deploys it with Docker.

It's built for one person running their own projects on their own hardware, such as a homelab, a spare PC or a small Debian box behind a Cloudflare tunnel. There's no cloud CI service to pay for or trust, and no SSH session to run `git pull` in. Each version's changes are listed in [CHANGELOG.md](CHANGELOG.md).

## Features

- **Watches your repositories.** Every few seconds it checks each one for a new version tag, or for a new commit on `main` if the project prefers that.
- **Deploys safely.** The running version keeps serving while the new one downloads and builds. If the new one doesn't come up healthy, the old one is put back. A commit that keeps failing is set aside until a fix arrives.
- **Checks before it deploys.** A compose file that would reach outside its project (root on the host, another project's files or secrets, the Docker socket) is refused before anything is built, and a Dockerfile `test` stage runs first if there is one. Exceptions are written down, one service at a time, in [`policy.json`](policy.json).
- **Keeps secrets out of your code.** A project's compose file names the secrets it needs, such as `${MYAPP_DATABASE_URL}`, and Lighthouse fetches them from [Cove](https://github.com/LSariol/Cove), a small self-hosted vault, at deploy time. Nothing secret lives in the repository or on disk, and the app reads ordinary environment variables.
- **Deploys infrastructure first.** The database and the vault deploy before the apps, one at a time, after a database backup.
- **Brings things back.** If a project's containers disappear, Lighthouse deploys what was running again.
- **Publishes your releases.** Once a version is deployed and healthy, Lighthouse publishes its GitHub Release, with that version's notes from `CHANGELOG.md`. Libraries that aren't deployed can have their Releases published too.
- **Updates itself.** A Lighthouse release is deployed like any project, through a short-lived helper container that puts the old version back if the new one isn't healthy.
- **Goes back when you ask.** `rollback <name>` returns to what ran before, and it stays there until something newer is released.
- **Has a control prompt.** List projects, see what's running, read logs, deploy, check a version without deploying it, see how past deploys went, or pause everything.
- **Remembers what happened.** Every deploy, step by step, is recorded in a small Postgres database.

```text
   You tag v1.2.0
         │
         ▼
      GitHub  ◄──── Lighthouse checks for new releases
                         │
                         ├──► Cove: "what are this project's secrets?"
                         │
                         └──► Docker: test, build and start the new version
                                    │
                                    ▼
                       Your apps, running on your server
```

Lighthouse, Cove and the apps all run as Docker containers on the same server, on a shared Docker network called `spark`.

## Requirements

- A Linux server with Docker and Docker Compose.
- A Postgres server for Lighthouse's own database.
- A running [Cove](https://github.com/LSariol/Cove) on the `spark` network, at `http://cove:2100`.
- A fine-grained GitHub token that can read your repositories (contents and metadata, read-only).
- Optionally, a GitHub App that can write repository contents, if Lighthouse should publish GitHub Releases.
- Go 1.27.4, only to build or work on Lighthouse itself.

## Getting started

### 1. Prepare the server

On the server, create the folders Lighthouse keeps its state in, and the shared network:

```bash
sudo mkdir -p /srv/server/storage/lighthouse/cove /srv/server/staging /srv/backups
docker network inspect spark >/dev/null 2>&1 || docker network create spark
```

### 2. Set up the database

Lighthouse uses one database, `lighthouse_db`, with four roles: `lighthouse_owner` (owns everything, can't log in), `lighthouse_migrator` (runs the migrations), `lighthouse_app` (Lighthouse at runtime) and `lighthouse_reader` (read-only, for a tool like pgAdmin). [`scripts/db/setup.sql`](scripts/db/setup.sql) creates whatever is missing and is safe to run again. It never changes the password of a role that already exists. Run it as a Postgres superuser, with a password for each role it has to create:

```bash
psql -U postgres -d postgres \
  -v migrator_password="..." -v app_password="..." -v reader_password="..." \
  -f scripts/db/setup.sql
```

It ends with `Done.` and a table in which every row matches its `want` column. [`scripts/db/check.sql`](scripts/db/check.sql) shows the same table without changing anything. Lighthouse creates its tables itself, through migrations, at every start.

### 3. Store the secrets in Cove

Lighthouse reads its own secrets from Cove with a read-only token over every key, which it fetches through Cove's bootstrap endpoint on its first start. In Cove's shell (`docker exec -it cove /cove shell`):

```text
token create lighthouse --allow '*'
create LIGHTHOUSE_GITHUB_TOKEN <the GitHub token>
create LIGHTHOUSE_DATABASE_URL postgres://lighthouse_app:<password>@<postgres host>:5432/lighthouse_db?sslmode=disable
create LIGHTHOUSE_DATABASE_MIGRATOR_URL postgres://lighthouse_migrator:<password>@<postgres host>:5432/lighthouse_db?sslmode=disable
bootstrap open lighthouse
```

`bootstrap open` lets Lighthouse collect its token within the next 10 minutes. A password must be letters and digits only, since it sits inside a URL.

### 4. Start Lighthouse

```bash
git clone https://github.com/lsariol/lighthouse.git
cd lighthouse
git checkout v1.4.0      # the newest release
LIGHTHOUSE_DEPLOY_VERSION=v1.4.0 docker compose up -d --build
docker logs -f lighthouse
```

The log shows `Lighthouse ready` once it has its Cove token and its database. `docker exec lighthouse /lighthouse status` then exits with status 0, or lists what needs attention.

Open the control prompt with `docker exec -it lighthouse /lighthouse shell`. `help` lists every command, and `exit` leaves the prompt while Lighthouse keeps running. Single commands work too, such as `docker exec lighthouse /lighthouse list`. To have Lighthouse update itself whenever a new release is tagged, add its own repository: `add https://github.com/lsariol/lighthouse`.

### 5. Add a project

A repository needs a compose file at its top (`compose.yaml` or `docker-compose.yml`) that builds and runs it. Lighthouse runs every service in it.

```yaml
name: myapp
x-lighthouse:
  deploy: releases                         # deploy version tags (v1.2.3), not every commit

services:
  web:
    build: .
    container_name: myapp-web
    restart: unless-stopped
    environment:
      - DATABASE_URL=${MYAPP_DATABASE_URL}   # a secret: fetched from Cove at deploy
      - API_KEY=${SHARED_EXAMPLE_API_KEY}    # a secret several projects share
      - LOG_LEVEL=info                       # a plain setting: written as a value
      - APP_VERSION=${LIGHTHOUSE_DEPLOY_VERSION:-dev}
    volumes:
      - /srv/server/storage/myapp/data:/app/data   # data: in the project's own folder
    healthcheck:
      test: ["CMD", "wget", "-qO-", "http://localhost:3000/health"]
      interval: 10s
      retries: 3
    networks: [spark]

networks:
  spark:
    external: true
```

Then, at the prompt, `add https://github.com/you/myapp`. The project is named after the repository. It deploys at its next version tag, once that tag's commit is on `main`. `check myapp` runs the deploy rules and the test stage first, without deploying anything.

What Lighthouse expects of a project:

- **Secrets are `${KEY}` without a default**, stored in Cove under exactly that name. A project may only use its own keys, which start with its compose project's name in capitals (`MYAPP_`), and shared ones (`SHARED_`). Anything with a default, such as `${KEY:-value}`, is a setting and isn't fetched.
- **Data lives in the project's own folder**, `/srv/server/storage/<compose project>/`, or in a named volume. A deploy that mounts any other host path, uses `privileged`, `cap_add`, devices or the host's namespaces, or joins another project's network or volume is refused. `help rules` lists every rule.
- **`name:` at the top** names the compose project, which Lighthouse finds the containers by. Two projects can't share one.
- **A healthcheck on every service that serves something** lets Lighthouse tell whether a new version really works before it keeps it. Without one, a service only has to stay running for 10 seconds.
- **`restart: unless-stopped`** on long-running services. A service without a restart policy is treated as a one-off job.
- **Names on `spark` are unique.** A service name, `container_name` or alias that another project already uses on `spark` would answer for both, so it's refused.
- **A Dockerfile stage named `test`** (`FROM … AS test`) is built before every deploy, without secrets, and a failure stops the deploy.
- **A service reached from outside** carries its public address as a label, `com.mobasity.public-url: https://myapp.example.com`, for monitoring and a status page.

The `x-lighthouse` block holds a project's own settings:

| Setting | Values | What it does |
|---|---|---|
| `deploy` | `branch` (default), `releases` | `releases` deploys each new version tag once its commit is on the default branch; `branch` deploys every commit on it |
| `tier` | `app` (default), `infra`, `data` | The deploy order: `data` first, then `infra`, then apps |
| `backup` | `postgres` | Dumps the project's Postgres service to `/srv/backups/<compose project>/` before each deploy |

Every deploy also gives the compose file `LIGHTHOUSE_DEPLOY_VERSION` (the release, or the commit's first 7 characters) and `LIGHTHOUSE_DEPLOY_COMMIT`, and labels each container `com.mobasity.version`.

### Publishing GitHub Releases

1. On GitHub, create a GitHub App you own, with no webhook and one repository permission, **Contents: Read and write**.
2. Generate a private key on the App's page, and install the App on the repositories whose Releases Lighthouse should publish.
3. Store the whole `.pem` file in Cove, base64-encoded, as `LIGHTHOUSE_GITHUB_APP_SECRET`, then delete the file.
4. Set `GITHUB_APP_ID` to the App's ID in `docker-compose.yml`, and deploy Lighthouse.

After that, each version Lighthouse deploys and verifies gets its Release, with the version's section of `CHANGELOG.md` (`## v1.2.0 (2026-10-20)`) as its text. `publish <name> <tag>` publishes an older tag by hand.

## Configuration

Lighthouse reads its settings from the environment. In Docker they're written out in `docker-compose.yml`. None of them is a secret.

| Variable | Default | Meaning |
|---|---|---|
| `COVE_URL` | | Cove's address, `http://cove:2100` |
| `COVE_TOKEN_PATH` | | Where Lighthouse keeps its Cove token |
| `STAGING_PATH` | | The deploy folders. In Docker, mounted at the same path as on the host |
| `STORAGE_PATH` | `/srv/server/storage` | The projects' data folders, mounted read-only to check symlinks |
| `BACKUP_PATH` | `/srv/backups` | Database backups taken before a deploy |
| `APP_ENV` | | `prod`, or `dev` for readable logs |
| `LOG_FORMAT` | `json` | `json` or `text` (`text` with `APP_ENV=dev`) |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `LIGHTHOUSE_VERSION` | `dev` | The version it reports |
| `LIGHTHOUSE_POLL_INTERVAL` | `10s` | How often GitHub is checked; at least `5s` |
| `LIGHTHOUSE_CONTROL_SOCKET` | `/run/lighthouse/control.sock` | Where the daemon listens for the command line |
| `GITHUB_APP_ID` | | The GitHub App that publishes Releases; unset, publishing is off |

Its secrets come from Cove:

| Key | What it is |
|---|---|
| `LIGHTHOUSE_GITHUB_TOKEN` | The GitHub token that reads repositories |
| `LIGHTHOUSE_DATABASE_URL` | The connection string for `lighthouse_app` |
| `LIGHTHOUSE_DATABASE_MIGRATOR_URL` | The connection string for `lighthouse_migrator` |
| `LIGHTHOUSE_GITHUB_APP_SECRET` | The GitHub App's private key, base64-encoded (only for publishing) |

## Everyday commands

| Command | What it does |
|---|---|
| `status` | Is everything healthy? |
| `list` | The projects being watched, and what each runs |
| `add <github-url>` | Start watching a project |
| `remove <name>` | Stop watching a project, and remove its containers |
| `deploy <name> [tag or commit]` | Deploy now, or deploy a given release or commit |
| `rollback <name>` | Go back to what ran before |
| `check <name> [tag or commit]` | Would it deploy? Changes nothing |
| `logs <name>` | A project's recent output |
| `history <name>` | How its recent deploys went; `report <name>` shows one step by step |
| `releases <name>` | Its version tags, and whether each one's GitHub Release is published |
| `pause` / `resume` | Hold all automatic deploys, or carry on |
| `help` | Everything else, with examples; `help setup` walks through preparing a repository |

## Development

To run Lighthouse on a development machine, copy `.env.example` to `.env` and point it at a development Cove. A local run keeps everything it writes in `.dev/`.

```bash
go run ./cmd/lighthouse          # the daemon, with its command line on this terminal
go run ./cmd/lighthouse help     # works without a daemon
```

The Dockerfile's `test` stage runs gofmt, `go vet` and the tests with the race detector, and Lighthouse builds it before every deploy of itself:

```bash
docker build --target test .
```

The database tests need a throwaway Postgres, which [`scripts/test-db.sh`](scripts/test-db.sh) starts. [`scripts/e2e-self-update.sh`](scripts/e2e-self-update.sh) runs a real self-update, with a rollback, against the local Docker.

```bash
scripts/test-db.sh up
eval "$(scripts/test-db.sh env)"
go test -race ./...
scripts/test-db.sh down
scripts/e2e-self-update.sh
```

## License

See [LICENSE](LICENSE).
