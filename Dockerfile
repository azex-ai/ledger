FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o ledgerd ./cmd/ledgerd

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=builder /app/ledgerd /usr/local/bin/
EXPOSE 8080
ENTRYPOINT ["ledgerd"]
