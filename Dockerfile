FROM golang:alpine as builder
WORKDIR /build
ADD . .
RUN go build -ldflags '-w -s'

FROM quay.io/prometheus/busybox:latest
COPY --from=builder /build/thanos-kit /bin/thanos-kit
ENTRYPOINT [ "/bin/thanos-kit" ]
