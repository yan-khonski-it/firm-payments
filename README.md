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

### Money representation and database constraints

Amounts are parsed from USD strings into integer cents without floating-point arithmetic. Go uses int64 for
calculations, while PostgreSQL uses INTEGER to match the assignment schema. Individual amounts and balances are limited
to 2,147,483,647 cents ($21,474,836.47); a payee balance overflow rolls back the entire batch.
Database constraints enforce non-negative balances, positive payment amounts, existing payer and payee references, no
self-payments, unique firm UUIDs, and descriptions of at most 500 characters. Required columns are NOT NULL. These
constraints complement request validation and protect the data from invalid writes.

### Transaction and concurrency strategy

Each payment batch runs in one PostgreSQL transaction at READ COMMITTED isolation. The service locks the payer and all
unique payees using SELECT ... ORDER BY id FOR UPDATE, then checks that every firm exists and the payer can cover the
entire batch.
It debits the payer once, credits each payee by its summed amount, and inserts one payment row per original entry.
Duplicate payees therefore retain separate payment records. All changes commit together; any failure before commit rolls
them back.
PostgreSQL row locks coordinate concurrent service instances without in-memory mutexes. Acquiring locks in ascending
firm ID order prevents circular waits between payment transactions following this strategy. Requests touching the same
firms may wait, then check the latest committed balances before proceeding.

### Timeouts and retries

Each request has an 8-second budget covering body reading, validation, database work, and retries. Transactions use a
2-second lock timeout, a 5-second statement timeout, and a 5-second idle transaction timeout. Response writing has an
additional 2-second allowance.

Deadlocks (`40P01`) and serialization failures (`40001`) retry the entire transaction up to three attempts, with
exponential backoff and jitter. Lock and statement timeouts are not retried. Exhausted retries or database timeouts
return `503` with `Retry-After`; a stalled request body returns `408`.

An unknown commit outcome (it means the service sent COMMIT but lost the connection before receiving confirmation.) is
never automatically retried: the payments may already have committed, and repeating them
could pay twice.

### Alternatives considered

### Alternatives considered

- **Optimistic locking:** use version checks and retry conflicting updates. This works well when conflicts are rare, but
  overlapping payment batches could require repeated work. I chose row locking to serialize access to shared balances.
- **`SERIALIZABLE` isolation:** provides stronger isolation, but requires handling serialization failures and retrying
  transactions. For this operation, `READ COMMITTED` with explicit locks on every affected firm provides the required
  correctness.
- **Payer-first locking with deadlock retries:** simpler initially, but opposing transfers can acquire locks in
  conflicting orders. I chose ascending firm ID order to prevent that circular-wait pattern, retaining retries as a
  safeguard.
- **In-memory mutexes:** coordinate requests within one process but cannot protect balances across independent service
  instances. PostgreSQL row locks provide coordination through the shared database.
- **Queue-based processing:** enqueue whole payment batches and process them
  sequentially. A single consumer simplifies concurrency but limits throughput
  and introduces queueing delays. Asynchronous processing would also require
  an acceptance response and a way to retrieve the final result. Multiple
  consumers still need database coordination, and redelivery requires
  idempotency. I chose synchronous database transactions to return `201` or
  `422` directly and keep the implementation small.

## Limitations and possible improvements

### Contention and scalability

## Limitations and possible improvements

### Contention and scalability

Requests involving the same payer or payee compete for row locks and are processed serially where they overlap. A
frequently used firm can therefore become a bottleneck. Adding application instances increases capacity for independent
batches, but does not remove contention on shared firms.

The 1,000-payment limit bounds each batch’s lock footprint and work. Timeouts prevent prolonged waits, although requests
may receive `503` under contention. Each instance also has a bounded connection pool; the combined pools must fit within
PostgreSQL’s connection capacity.

Possible improvements include measuring lock waits and transaction duration, reducing database round trips, and
introducing admission control or queueing to absorb bursts. More substantial changes, such as partitioning or a
ledger-based design, would require revisiting how cross-firm payments remain atomic.

A ledger-based design records each payment as linked debit and credit entries, committed atomically. It provides a clear
payment history for auditing.
If receiver balances are calculated from the ledger, payments can append credit entries without updating a shared
receiver balance row. This can improve receiver-heavy workloads, but it does not automatically make every payment
faster: preventing payer overspending still requires concurrency controls.

### Other production improvements

- **Idempotency:** prevent duplicate payments when clients retry after losing a response.
- **Authentication and authorization:** verify the caller and their permission to spend from the payer.
- **Observability:** add structured logs, request IDs, and metrics for latency, lock waits, retries, and failures.
- **Operational reliability:** add readiness checks, database backups, and tested recovery procedures.
- **Abuse protection:** apply rate limits and bound concurrent requests to protect database capacity.
- **Reconciliation:** provide a way to inspect recorded payments and investigate unknown commit outcomes.

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
I shared relevant context and feedback between these conversations and remained responsible for the decisions and final
implementation.

I reviewed the implementation, although my manual review of the test code was limited.

The prompts and recorded responses are available in the [AI-prompts directory](./AI-prompts).
These records include summaries and should not be treated as complete raw interaction logs.