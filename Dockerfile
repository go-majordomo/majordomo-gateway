FROM golang:1.25-alpine AS builder

WORKDIR /app

RUN apk add --no-cache ca-certificates

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /app/gateway ./cmd/gateway && \
    CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /app/gateway-cli ./cmd/gateway-cli && \
    CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /app/gateway-mcp ./cmd/gateway-mcp

FROM alpine:3.19

WORKDIR /app

RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /app/gateway /app/gateway
COPY --from=builder /app/gateway-cli /app/gateway-cli
COPY --from=builder /app/gateway-mcp /app/gateway-mcp
COPY pricing.json /app/pricing.json
COPY model_aliases.json /app/model_aliases.json
COPY deprecated_models.json /app/deprecated_models.json
COPY migrations/ /app/migrations/

RUN adduser -D -u 1000 majordomo
USER majordomo

EXPOSE 6560

# Runs migrations on startup, then serves.
ENTRYPOINT ["/app/gateway"]
