# firm-payments

Implement payment service that receives a POST request with payments from source accounts
to multiple destination accounts.

## Prerequisites:
- go lang, I used go1.26.3
- curl
- Docker, needed to run Postgres or Postgres.

## Setup:
clone the repository:
```shell
git clone https://github.com/yan-khonski-it/firm-payments.git
```

Run dependencies, PostgreSQL:
```shell
docker compose up -d
export DATABASE_URL="postgres://app:app@localhost:5432/orders?sslmode=disable"
```

On Windows machine to set the URL:
```shell
$env:DATABASE_URL = "postgres://app:app@localhost:5432/orders?sslmode=disable"
```

Apply migrations:
```shell
go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@latest
go run -tags postgres github.com/golang-migrate/migrate/v4/cmd/migrate@latest -path ./sql -database $env:DATABASE_URL up
```
Run the service:
```shell
go run ./cmd/server
```