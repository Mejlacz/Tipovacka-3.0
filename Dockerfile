# ── Build stage ──────────────────────────────────────────────────────────────
FROM golang:1.24-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o tipovacka .

# ── Runtime stage ─────────────────────────────────────────────────────────────
FROM alpine:3.19

RUN apk add --no-cache tzdata ca-certificates
WORKDIR /app

COPY --from=builder /app/tipovacka .
COPY --from=builder /app/templates ./templates
COPY --from=builder /app/static    ./static

EXPOSE 8080
ENV PORT=8080

CMD ["./tipovacka"]
