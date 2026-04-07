FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /activity-service .

FROM scratch
COPY --from=builder /activity-service /activity-service
EXPOSE 8080
ENTRYPOINT ["/activity-service"]
