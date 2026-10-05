# firm-payments

A Go service that atomically executes bulk payments from one firm to multiple
firms, with concurrency handled through PostgreSQL across service instances. See
[API and validation](#api-and-validation) for the request format and responses.

## Prerequisites

- Go 1.26 or newer; tested with Go 1.26.3.
- curl
- Docker Compose for the PostgreSQL setup below, or an existing PostgreSQL
  server.

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

This runs unit tests without Docker or PostgreSQL when `TEST_DATABASE_URL` is
unset. If that variable is set in the current terminal, integration tests also
run. The `-v` flag shows `SKIP` lines for integration tests that did not run. To
run only unit tests, unset it first:

Windows (PowerShell):

```powershell
Remove-Item Env:TEST_DATABASE_URL -ErrorAction SilentlyContinue
```

Linux/macOS (Bash or Zsh):

```bash
unset TEST_DATABASE_URL
```

### Run integration tests

Integration tests require a running PostgreSQL server and a migrated test
database. They do not start PostgreSQL, create the database, or apply schema
migrations. The application server does not need to be running.

Use a TCP URL with an explicit host and port for `TEST_DATABASE_URL`, as shown
below. The lost-commit proxy tests skip when given a `key=value` connection
string.

**Integration tests delete and reseed all rows in `firms` and `payments` in the
selected database. Use a disposable test database, never a database with data
you want to keep.**

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

### Run PostgreSQL and apply migrations

If port 5432 is already in use, follow the port-mapping instructions under [Run
integration tests](#run-integration-tests) and update `DATABASE_URL`
accordingly.

```shell
docker compose up -d db
```

Wait for PostgreSQL readiness as described above, then apply migrations to
`orders`.

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
`:8081`, if port 8080 is taken (`$env:ADDR = ":8081"` in PowerShell or `export
ADDR=":8081"` in Bash/Zsh). Update the port in the curl commands accordingly.

```shell
go run ./cmd/server
```

The server requires `DATABASE_URL` in the terminal where it starts and checks
database connectivity at startup. A missing URL or an unreachable database
causes startup to fail. Apply the migrations before sending requests.

A successful payment batch returns **201 Created**. Missing firms return
**404**, insufficient funds return **422**, and temporary database failures
return **503**.

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

### Verify the result

Starting from freshly seeded data, send `payment-body.json` once, then query the
balances:

```shell
docker compose exec db psql -U app -d orders -c "SELECT id, name, balance_cents FROM firms ORDER BY id"
```

| Firm | Balance after one successful request (cents) |
| --- | ---: |
| Pinecrest CPA Group | 3,674,875 |
| Lopez Bookkeeping | 170,075 |
| Nair Tax Services | 1,405,050 |

Send the same request three more times. Across all four submissions, expect
`201`, `201`, `201`, then `422`: the initial balance covers only three batches.
After those four attempts, balances are 1,024,625 / 410,225 / 3,815,150 cents
respectively, with 9 payment rows. The rejected batch changes nothing.

```shell
docker compose exec db psql -U app -d orders -c "SELECT count(*) FROM payments"
```

## API and validation

`POST /payments` accepts `Content-Type: application/json`, including parameters
such as `charset=utf-8`. Example request:

```json
{
  "payer_firm_uuid": "3f1c9a2e-7b4d-4c1e-9a55-2d8e6f0b7c41",
  "payments": [
    {
      "amount": "1200.75",
      "payee_firm_uuid": "8b2e4c71-0d3a-4f6e-b1c9-5a7d2e9f4c10",
      "description": "Bookkeeping cleanup, 3 clients"
    }
  ]
}
```

- The body must be a single JSON object, at most 4 MiB; unknown fields are
  rejected.
- Payer and payee UUIDs are required strings in hyphenated UUID format;
  uppercase hexadecimal is accepted and normalized to lowercase.
- `payments` must contain 1–1,000 entries. Self-payments are rejected; duplicate
  payees are allowed and retain separate payment records.
- Each `amount` is a positive dollar string with at most two decimal places,
  such as `"300"`, `"5800.5"`, or `"1200.75"`. The maximum is `"21474836.47"`.
- Each `description` is a required string of at most 500 Unicode characters. An
  empty string is allowed; `null` and NUL characters are rejected.
- Firm existence and sufficient funds for the entire batch are checked inside
  the database transaction.

The batch-size and description limits, rejection of self-payments, and
additional validation rules are implementation choices. The assignment requires
`201` on success and `422` for insufficient funds; the other statuses are API
choices.

A successful request returns `201` with this shape (for the example above):

```json
{
  "payer_firm_uuid": "3f1c9a2e-7b4d-4c1e-9a55-2d8e6f0b7c41",
  "payment_count": 1,
  "total_amount": "1200.75"
}
```

Errors use a JSON envelope with `error.code`, `error.message`, and an optional
`error.field` identifying the invalid request field. For example:

```json
{
  "error": {
    "code": "payer_not_found",
    "message": "the payer firm does not exist; nothing was paid",
    "field": "payer_firm_uuid"
  }
}
```

| Status | Error codes | Meaning |
| --- | --- | --- |
| `400` | Validation codes listed below | Malformed JSON or invalid request fields |
| `404` | `payer_not_found`, `payee_not_found`, `not_found` | Unknown payer, payee, or endpoint |
| `405` | `method_not_allowed` | Unsupported method on `/payments`; `Allow: POST` |
| `408` | `request_timeout` | Request body did not arrive within the request budget |
| `413` | `request_too_large` | Request body exceeds 4 MiB |
| `415` | `unsupported_media_type` | Missing or unsupported Content-Type |
| `422` | `insufficient_funds`, `balance_limit_exceeded` | Insufficient funds or a payee balance would exceed the supported limit |
| `503` | `busy`, `timeout` | Database busy or request deadline exceeded; `Retry-After: 1` |
| `500` | `outcome_unknown`, `internal_error` | Lost commit reply or unexpected error |

The `400` validation codes are `invalid_json`, `unknown_field`, `missing_field`,
`invalid_type`, `invalid_uuid`, `invalid_amount`, `amount_too_large`,
`description_too_long`, `invalid_character`, `no_payments`, `too_many_payments`,
and `self_payment`.

## Solution approach

### Database driver

The service uses Go's `database/sql` with `lib/pq` rather than `pgx`. I chose
`lib/pq` because it has no additional dependencies, keeping the dependency set
small while supporting the PostgreSQL transactions and explicit SQL used here.

### Money representation and database constraints

Amounts are parsed from USD strings into integer cents without floating-point
arithmetic. Go uses int64 for calculations, while PostgreSQL uses INTEGER to
match the assignment schema. Individual amounts and balances are limited to
2,147,483,647 cents ($21,474,836.47); a payee balance overflow rolls back the
entire batch. Database constraints enforce non-negative balances, positive
payment amounts, existing payer and payee references, no self-payments, unique
firm UUIDs, and descriptions of at most 500 characters. Required columns are NOT
NULL. These constraints complement request validation and protect the data from
invalid writes.

### Transaction and concurrency strategy

Each payment batch runs in one PostgreSQL transaction at READ COMMITTED
isolation. The service locks the payer and all unique payees using SELECT ...
ORDER BY id FOR UPDATE, then checks that every firm exists and the payer can
cover the entire batch. It debits the payer once, credits each payee by its
summed amount, and inserts one payment row per original entry. Duplicate payees
therefore retain separate payment records. All changes commit together; any
failure before commit rolls them back. PostgreSQL row locks coordinate
concurrent service instances without in-memory mutexes. Acquiring locks in
ascending firm ID order prevents circular waits between payment transactions
following this strategy. Requests touching the same firms may wait, then check
the latest committed balances before proceeding.

### Timeouts and retries

Each request has an 8-second budget covering body reading, validation, database
work, and retries. Transactions use a 2-second lock timeout, a 5-second
statement timeout, and a 5-second idle transaction timeout. Response writing has
an additional 2-second allowance.

Deadlocks (`40P01`) and serialization failures (`40001`) retry the entire
transaction up to three attempts (one initial attempt and at most two retries),
with exponential backoff and jitter. Lock and statement timeouts are not
retried. Exhausted retries or database lock and statement timeouts return `503`
with code `busy` and `Retry-After: 1`.

If the 8-second request budget expires during database work or retry backoff,
the service returns `503` with code `timeout` and `Retry-After: 1`. A timeout
while reading the request body returns `408` with code `request_timeout`. An
unknown commit outcome is handled separately, as described below.

An unknown commit outcome means the service sent `COMMIT` but lost the
connection before receiving confirmation. The service logs the error and returns
`500` with code `outcome_unknown`. It never automatically retries the
transaction because the payments may already have committed, and repeating them
could pay twice.

### Alternatives considered

- **Optimistic locking:** use version checks and retry conflicting updates. This
  works well when conflicts are rare, but overlapping payment batches could
  require repeated work. I chose row locking to serialize access to shared
  balances.
- **`SERIALIZABLE` isolation:** provides stronger isolation, but requires
  handling serialization failures and retrying transactions. For this operation,
  `READ COMMITTED` with explicit locks on every affected firm provides the
  required correctness.
- **Payer-first locking with deadlock retries:** simpler initially, but opposing
  transfers can acquire locks in conflicting orders. I chose ascending firm ID
  order to prevent that circular-wait pattern, retaining retries as a safeguard.
- **In-memory mutexes:** coordinate requests within one process but cannot
  protect balances across independent service instances. PostgreSQL row locks
  provide coordination through the shared database.
- **Queue-based processing:** enqueue whole payment batches and process them
  sequentially. A single consumer simplifies concurrency but limits throughput
  and introduces queueing delays. Asynchronous processing would also require an
  acceptance response and a way to retrieve the final result. Multiple consumers
  still need database coordination, and redelivery requires idempotency. I chose
  synchronous database transactions to return `201` or `422` directly and keep
  the implementation small.

## Issues encountered

- **Retries can hide a lock-order regression:** successful payments alone do
  not prove deadlocks were avoided, because retries can recover from them. The
  opposing-transfer test therefore also requires zero retries. Ordered locking
  prevents circular waits; the assertion helps detect a regression.
- **Slow-body test failure observed on Windows:** writing the remaining body
  after the server timed out caused a connection reset that hid the unread
  response. The test now reads the response while the upload is stalled.
- **Integration tests sharing one database:** Go runs test packages in parallel,
  so their resets could interfere with each other. A PostgreSQL advisory lock
  serializes database tests. Lock acquisition has a separate timeout, so waiting
  does not consume the test's execution budget.
- **Covering body reading with the request deadline:** the request budget must
  start before receiving the body, with time left to write a response. Connection
  deadlines bound slow uploads, while the request context bounds database work;
  see
  [Timeouts and retries](#timeouts-and-retries) for the limits and responses.
- **Lost commit confirmation:** a connection failure during commit leaves the
  result uncertain. The service distinguishes this from a confirmed failure and
  never automatically retries it, because the batch may already have committed.

## Limitations and possible improvements

### Unicode decoding

Invalid UTF-8 bytes are rejected, but unpaired UTF-16 surrogate escapes such as
`\ud800` are replaced with `�` (U+FFFD) by Go's JSON decoder. A description
containing such an escape can therefore be stored with altered text. A future
improvement would reject unpaired surrogate escapes during request validation.

### Database calls and the request budget

`lib/pq` does not fully honor request contexts: `BEGIN` runs before it starts
watching the context, and for later statements a cancelled context only sends
PostgreSQL a separate cancel request, which an unresponsive server never
answers. To bound such waits, every database connection limits each read and
each write to 7 seconds (`DefaultIOTimeout`), above the 5-second statement
timeout so that PostgreSQL's own timeouts fire first while it is responsive.

This limit applies to each read and write, not to the whole request. If the
database stops answering or stops reading, a call can run up to 7 seconds past
the remaining request budget. A database that keeps exchanging occasional bytes
without finishing could extend it further. If such a stall happens before
`COMMIT`, nothing is paid; a stall during `COMMIT` is reported as an unknown
outcome. Either way, the client may receive no response.

A future improvement would pass each request's deadline to its database
connection, or switch to `pgx`, which applies the context deadline to the
socket itself.

### Contention and scalability

Requests involving the same payer or payee compete for row locks and are
processed serially where they overlap. A frequently used firm can therefore
become a bottleneck. Adding application instances increases capacity for
independent batches, but does not remove contention on shared firms.

The 1,000-payment limit bounds each batch's lock footprint and work. Timeouts
prevent prolonged waits, although requests may receive `503` under contention.
Each instance also has a bounded connection pool; the combined pools must fit
within PostgreSQL's connection capacity.

Possible improvements include measuring lock waits and transaction duration,
reducing database round trips, and introducing admission control or queueing to
absorb bursts. More substantial changes, such as partitioning or a ledger-based
design, would require revisiting how cross-firm payments remain atomic.

A ledger-based design records each payment as linked debit and credit entries,
committed atomically. It provides a clear payment history for auditing. If
receiver balances are calculated from the ledger, payments can append credit
entries without updating a shared receiver balance row. This can improve
receiver-heavy workloads, but it does not automatically make every payment
faster: preventing payer overspending still requires concurrency controls.

### Other production improvements

- **Idempotency:** prevent duplicate payments on retries; see
  [Idempotency](#idempotency).
- **Authentication and authorization:** verify the caller and their permission
  to spend from the payer.
- **Observability:** add structured logs, request IDs, and metrics for latency,
  lock waits, retries, and failures.
- **Operational reliability:** add readiness checks, database backups, and
  tested recovery procedures.
- **Abuse protection:** apply rate limits and bound concurrent requests to
  protect database capacity.
- **Reconciliation:** provide a way to inspect recorded payments and investigate
  unknown commit outcomes.

### Idempotency

Idempotency is not supported. Every request is treated as a new payment batch,
so resubmitting the same request may execute it again. If a request times out or
the client connection fails, the transaction may still commit even though the
client receives no response. Retrying the request could then execute the
payments twice.

The service does not retry transactions with an unknown commit outcome, but it
cannot prevent client retries. A future implementation could accept an
Idempotency-Key and store it atomically with the payments and result, allowing
repeated requests to return the stored result without executing the batch again.

## AI assistance

I used Claude Opus 5.5, OpenAI models (mostly Sol 6.1), and GitHub Copilot to
assist with requirements, implementation, tests, and code review. The prompt
records also identify GPT 5.6 Sol used through GitHub Copilot; this refers to a
different model used during development.

I coordinated several model conversations in parallel. While one model
implemented a task, I reviewed another part of the solution with a second model
and used GitHub Copilot to check earlier changes. I shared relevant context and
feedback between these conversations and remained responsible for the decisions
and final implementation.

I reviewed the implementation, although my manual review of the test code was
limited.

The prompts and recorded responses are available in the [AI-prompts
directory](./AI-prompts). These records include summaries and should not be
treated as complete raw interaction logs.
