# syntax=docker/dockerfile:1
# Build
FROM golang:1.25-bookworm AS builder

# TLS: refresh public roots; optional MITM roots — add *.crt under deploy/docker/extra-ca/ before build.
RUN apt-get update \
	&& apt-get install -y --no-install-recommends ca-certificates \
	&& rm -rf /var/lib/apt/lists/*

COPY deploy/docker/extra-ca/ /tmp/extra-ca/
RUN set -eux; \
	count="$(find /tmp/extra-ca -maxdepth 1 -name '*.crt' -type f | wc -l)"; \
	if [ "$count" -gt 0 ]; then \
		find /tmp/extra-ca -maxdepth 1 -name '*.crt' -type f -exec cp -t /usr/local/share/ca-certificates/ {} +; \
		update-ca-certificates; \
	fi

WORKDIR /src

COPY go.mod go.sum ./
COPY vendor/ vendor/

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -mod=vendor -ldflags="-s -w" -o /server ./cmd/server

# Runtime
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

COPY --from=builder /server /app/server
COPY migrations /app/migrations

ENV MIGRATIONS_PATH=/app/migrations

USER nonroot:nonroot

EXPOSE 8080

ENTRYPOINT ["/app/server"]
