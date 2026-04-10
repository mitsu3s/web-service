FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /web-bff .

FROM scratch
LABEL org.opencontainers.image.description="Devboard web BFF"
COPY --from=builder /web-bff /web-bff
EXPOSE 8080
ENTRYPOINT ["/web-bff"]
