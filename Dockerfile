# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY . .
RUN go build -o healthchecker cmd/main.go

FROM scratch
COPY --from=builder /app/healthchecker /healthchecker
EXPOSE 8080
ENTRYPOINT ["/healthchecker"]