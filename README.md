# concerts-Go (shamil sharipov)

REST API сервер на Go + Postgres для управления концертами/шоу/местами.

## Быстрый старт

1) Поднять Postgres и применить миграции:

```bash
docker compose up -d
```

2) Запустить API:

```bash
set DATABASE_URL=postgres://postgres:postgres@localhost:5432/concerts?sslmode=disable
go run ./cmd/api
```

## API

- `GET /api/v1/concerts`
- `GET /api/v1/concerts/{concert-id}`
- `GET /api/v1/concerts/{concert-id}/shows/{show-id}/seating`

Content-Type ответа: `application/json`

