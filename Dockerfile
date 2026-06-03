FROM golang:1.25 AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /pufferfs-server ./cmd/server

FROM gcr.io/distroless/static:nonroot

COPY --from=builder /pufferfs-server /pufferfs-server
COPY --from=builder /app/migrations /migrations

ENV MIGRATIONS_DIR=/migrations

EXPOSE 8080

USER nonroot:nonroot
ENTRYPOINT ["/pufferfs-server"]
