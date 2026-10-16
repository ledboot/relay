FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /server .
FROM alpine:3.22
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 app && mkdir /data && chown app:app /data
COPY --from=build /server /server
USER app
ENV LISTEN_ADDR=0.0.0.0:8080 DATABASE_PATH=/data/sms.db
ENTRYPOINT ["/server"]
