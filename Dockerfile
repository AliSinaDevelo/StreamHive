# syntax=docker/dockerfile:1

FROM golang:1.22-alpine AS build
WORKDIR /src
RUN apk add --no-cache ca-certificates
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/streamhive .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/streamhive /usr/local/bin/streamhive
USER nobody
EXPOSE 7070 8080
ENTRYPOINT ["/usr/local/bin/streamhive"]
