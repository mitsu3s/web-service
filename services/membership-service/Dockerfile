FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /membership-service .

FROM scratch
LABEL org.opencontainers.image.description="Devboard membership service"
COPY --from=builder /membership-service /membership-service
EXPOSE 8080
ENTRYPOINT ["/membership-service"]
