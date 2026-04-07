FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /project-service .

FROM scratch
COPY --from=builder /project-service /project-service
EXPOSE 8080
ENTRYPOINT ["/project-service"]
