FROM golang:1.27-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/erp-inventory ./cmd/server

FROM alpine:3.21
RUN adduser -D -H -u 10001 appuser
WORKDIR /app
COPY --from=builder /out/erp-inventory /app/erp-inventory
USER appuser
EXPOSE 8080
ENTRYPOINT ["/app/erp-inventory"]