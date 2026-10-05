# firm-payments

Implement payment service that receives a POST request with payments from source accounts
to multiple destination accounts.

## Prerequisites

- Go 1.26 or newer; tested with Go 1.26.3.
- curl
- Docker Compose for the PostgreSQL setup below, or an existing PostgreSQL server.

Run all commands below from the repository root.

## Setup

Clone the repository:
```shell
git clone https://github.com/yan-khonski-it/firm-payments.git
cd firm-payments
```

### Run unit tests

```shell
go test -v ./...
```

This runs unit tests without Docker or PostgreSQL when `TEST_DATABASE_URL` is unset.
If that variable is set in the current terminal, integration tests also run.
The `-v` flag shows `SKIP` lines for integration tests that did not run.
To run only unit tests, unset it first:

Windows (PowerShell):

```powershell
Remove-Item Env:TEST_DATABASE_URL -ErrorAction SilentlyContinue
```

Linux/macOS (Bash or Zsh):

```bash
unset TEST_DATABASE_URL
```

### Run integration tests

Integration tests require a running PostgreSQL server and a migrated test database.
They do not start PostgreSQL, create the database, or apply schema migrations.
The application server does not need to be running.

Use a TCP URL with an explicit host and port for `TEST_DATABASE_URL`, as shown
below. The lost-commit proxy tests skip when given a `key=value` connection string.

**Integration tests delete and reseed all rows in `firms` and `payments` in the
selected database. Use a disposable test database, never a database with data you
want to keep.**

```shell
docker compose up -d db
```

If host port 5432 is already in use, change the mapping in `docker-compose.yml`
to, for example, `"5433:5432"`, and use port 5433 in both `DATABASE_URL` and
`TEST_DATABASE_URL`. Commands executed inside the container still use port 5432.

Wait until PostgreSQL is ready before continuing. Run this check again if it
reports that PostgreSQL is not accepting connections yet:

```shell
docker compose exec db pg_isready -h 127.0.0.1 -U app -d orders
```

The TCP check avoids reporting readiness during the image's temporary
initialization server, which listens only on a Unix socket.

Compose creates `orders`; create the separate `orders_test` database once:

```shell
docker compose exec db createdb -U app orders_test
```

If `orders_test` already exists, skip the creation command. Running it again
reports an "already exists" error.

Windows (PowerShell):

```powershell
$env:TEST_DATABASE_URL = "postgres://app:app@localhost:5432/orders_test?sslmode=disable"

go run -tags postgres github.com/golang-migrate/migrate/v4/cmd/migrate@v4.20.1 -path ./sql -database $env:TEST_DATABASE_URL up

go test -count=1 -v ./...
```

Linux/macOS (Bash or Zsh):

```bash
export TEST_DATABASE_URL="postgres://app:app@localhost:5432/orders_test?sslmode=disable"

go run -tags postgres github.com/golang-migrate/migrate/v4/cmd/migrate@v4.20.1 \
  -path ./sql \
  -database "$TEST_DATABASE_URL" up

go test -count=1 -v ./...
```

### Run dependencies, PostgreSQL

If port 5432 is already in use, follow the port-mapping instructions under
[Run integration tests](#run-integration-tests) and update `DATABASE_URL` accordingly.

```shell
docker compose up -d db
```

Wait for PostgreSQL readiness as described above, then apply migrations to `orders`.

Windows (PowerShell):

```powershell
$env:DATABASE_URL = "postgres://app:app@localhost:5432/orders?sslmode=disable"

go run -tags postgres github.com/golang-migrate/migrate/v4/cmd/migrate@v4.20.1 -path ./sql -database $env:DATABASE_URL up
```

Linux/macOS (Bash or Zsh):

```bash
export DATABASE_URL="postgres://app:app@localhost:5432/orders?sslmode=disable"

go run -tags postgres github.com/golang-migrate/migrate/v4/cmd/migrate@v4.20.1 \
  -path ./sql \
  -database "$DATABASE_URL" up
```

### Run the service

The server listens on `:8080` by default. Set `ADDR` to another address, such as
`:8081`, if port 8080 is taken (`$env:ADDR = ":8081"` in PowerShell or
`export ADDR=":8081"` in Bash/Zsh). Update the port in the curl commands accordingly.

```shell
go run ./cmd/server
```

The HTTP handler currently validates requests but is not wired to the database
store. A valid request returns **501 Not Implemented** and executes no payments.
The server currently does not read `DATABASE_URL` or require PostgreSQL to start.

Send a sample request from another terminal in the repository root:

Windows (PowerShell):

```powershell
curl.exe -i -X POST http://localhost:8080/payments -H "Content-Type: application/json; charset=utf-8" --data-binary "@payment-body.json"
```

Linux/macOS (Bash or Zsh):

```bash
curl -i -X POST http://localhost:8080/payments \
  -H "Content-Type: application/json; charset=utf-8" \
  --data-binary "@payment-body.json"
```


## Solution approach

### Architecture

### Money representation and database constraints

### Transaction and concurrency strategy

### Timeouts and retries

### Alternatives considered


## Limitations and possible improvements

### Contention and scalability

### Other production improvements

### Idempotency
Idempotency is not supported. Every request is treated as a new payment batch, so resubmitting the same request
may execute it again. If a request times out or the client connection fails,
the transaction may still commit even though the client receives no response.
Retrying the request could then execute the payments twice.

The service does not retry transactions with an unknown commit outcome,
but it cannot prevent client retries.
A future implementation could accept an Idempotency-Key and store it atomically with the payments and result,
allowing repeated requests to return the stored result without executing the batch again.


## AI assistance

I used Claude Opus 5.5, OpenAI models (mostly Sol 6.1), and GitHub Copilot to assist with requirements, 
implementation, tests, and code review.

I coordinated several model conversations in parallel. While one model implemented a task,
I reviewed another part of the solution with a second model and used GitHub Copilot to check earlier changes.
I shared relevant context and feedback between these conversations and remained responsible for the decisions and final implementation.

I reviewed the implementation, although my manual review of the test code was limited.

The prompts and recorded responses are available in the [AI-prompts directory](./AI-prompts).
These records include summaries and should not be treated as complete raw interaction logs.