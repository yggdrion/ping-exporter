FROM golang:1.23-alpine AS builder

WORKDIR /build

RUN apk add --no-cache git

COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./

RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o ping-exporter .

FROM alpine:latest

RUN apk --no-cache add ca-certificates tzdata iputils

WORKDIR /app

COPY --from=builder /build/ping-exporter .

COPY targets.json .

EXPOSE 9090

ENTRYPOINT ["/app/ping-exporter"]
