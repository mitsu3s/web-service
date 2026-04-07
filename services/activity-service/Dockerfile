FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /activity-service .

FROM scratch
LABEL org.opencontainers.image.description="Devboard activity service"
COPY --from=builder /activity-service /activity-service
EXPOSE 8080
ENTRYPOINT ["/activity-service"]
