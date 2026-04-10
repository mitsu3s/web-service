FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /workflow-service .

FROM scratch
LABEL org.opencontainers.image.description="Devboard workflow service"
COPY --from=builder /workflow-service /workflow-service
EXPOSE 8080
ENTRYPOINT ["/workflow-service"]
