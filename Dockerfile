FROM alpine:latest

ENTRYPOINT ["/nats-watch"]
COPY nats-watch /
