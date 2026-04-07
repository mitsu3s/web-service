FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /search-indexer .

FROM scratch
LABEL org.opencontainers.image.description="Devboard search indexer"
COPY --from=builder /search-indexer /search-indexer
EXPOSE 8080
ENTRYPOINT ["/search-indexer"]
