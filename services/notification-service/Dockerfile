FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /notification-service .

FROM scratch
COPY --from=builder /notification-service /notification-service
EXPOSE 8080
ENTRYPOINT ["/notification-service"]
