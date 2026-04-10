FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /task-command-service .

FROM scratch
LABEL org.opencontainers.image.description="Devboard task command service"
COPY --from=builder /task-command-service /task-command-service
EXPOSE 8080
ENTRYPOINT ["/task-command-service"]
