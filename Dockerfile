FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /gcsconduit .

FROM alpine:latest
COPY --from=builder /gcsconduit /gcsconduit
EXPOSE 8080
ENTRYPOINT ["/gcsconduit"]
