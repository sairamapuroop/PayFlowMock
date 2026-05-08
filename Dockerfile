# Build
FROM golang:1.25-bookworm AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /server ./cmd/server

# Runtime
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

COPY --from=builder /server /app/server
COPY migrations /app/migrations

ENV MIGRATIONS_PATH=/app/migrations

USER nonroot:nonroot

EXPOSE 8080

ENTRYPOINT ["/app/server"]
