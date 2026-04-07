FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /auth-service .

FROM scratch
LABEL org.opencontainers.image.description="Devboard authentication service"
COPY --from=builder /auth-service /auth-service
EXPOSE 8080
ENTRYPOINT ["/auth-service"]
