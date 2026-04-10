FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /access-service .

FROM scratch
LABEL org.opencontainers.image.description="Devboard access service"
COPY --from=builder /access-service /access-service
EXPOSE 8080
ENTRYPOINT ["/access-service"]
