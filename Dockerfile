FROM golang:alpine AS builder

RUN apk add --no-cache git gcc musl-dev

WORKDIR /app

COPY go.mod go.sum ./

RUN go mod download

COPY . .  

RUN go build -o /go/bin/app -v ./...

FROM alpine:latest

RUN apk --no-cache add ca-certificates

WORKDIR /app

COPY --from=builder /go/bin/app /app/app

COPY index.html ./index.html


ENTRYPOINT ["/app/app"]

LABEL Name=d Version=0.0.1

EXPOSE 3000