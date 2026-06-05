FROM golang:1.25 AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /pufferfs-server ./cmd/server
RUN CGO_ENABLED=0 GOOS=linux go build -o /pufferfs-worker ./cmd/worker
RUN CGO_ENABLED=0 GOOS=linux go build -o /pufferfs-runtime ./cmd/runtime

FROM gcr.io/distroless/static:nonroot

COPY --from=builder /pufferfs-server /pufferfs-server
COPY --from=builder /pufferfs-worker /pufferfs-worker
COPY --from=builder /pufferfs-runtime /pufferfs-runtime
COPY --from=builder /app/migrations /migrations

ENV MIGRATIONS_DIR=/migrations

EXPOSE 8080

USER nonroot:nonroot
CMD ["/pufferfs-runtime"]
