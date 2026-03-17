# Image Processing Service

## Overview

A stateless Dockerized Go service for image proxying + transformation + caching.

Features:
- User auth (register/login/JWT)
- Image upload, list/retrieve, transform
- Resize/crop/rotate/flip/mirror/watermark/grayscale/sepia
- Format conversion (PNG/JPEG/WEBP)
- In-memory transform cache (idle TTL)
- Error placeholders (4xx orange, 5xx red)
- Prometheus metrics + health/readiness
- Full stack orchestration with Docker Compose + Grafana/Loki/Prometheus/Redis/Postgres/Traefik

## Quickstart

```bash
cd '/Users/mekan/Documents/Projects/Ulker/Image Processing Tool'
docker compose up --build
```

API:
- `POST /register` with JSON username/password
- `POST /login`
- `POST /images` multipart body `image`
- `GET /images?page=1&limit=10`
- `GET /images/{id}?format=jpeg`
- `POST /images/{id}/transform` JSON operations

Observability:
- `GET /health`
- `GET /ready`
- `GET /metrics`
- Grafana: http://localhost:3000
- Prometheus: http://localhost:9090

## Concurrency + Rate Limiting

- Worker pool for multi-size transform:
  - request field `parallel_resizes` (array of `{width,height}`)
  - request field `max_workers` (worker concurrency limit)
  - image processing uses goroutines + `sync.WaitGroup` + channels
- Rate limiting:
  - configured with `RATE_LIMIT_PER_SEC` (default 20)
  - channel token bucket in middleware
  - excessive requests return HTTP 429
- Prometheus metrics:
  - `image_processing_api_rate_limited_total`
  - `image_processing_api_active_transform_workers`

## Config files

`config/` includes:
- `grafana-datasources.yaml`
- `loki.yaml`
- `prometheus.yaml`
- `promtail.yaml`
- `redis.conf`
- `postgresql.conf`
- `traefik.yml`

## Notes

- `go` command not available in this environment; run local Go toolchain for tests.
- `docker compose` is required to run full stack.
