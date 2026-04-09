FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /task-orchestrator-service .

FROM scratch
LABEL org.opencontainers.image.description="Devboard task orchestrator service"
COPY --from=builder /task-orchestrator-service /task-orchestrator-service
EXPOSE 8080
ENTRYPOINT ["/task-orchestrator-service"]
