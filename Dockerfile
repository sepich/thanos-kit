FROM golang:1.21 as builder
WORKDIR /build
# try to cache deps
COPY go.mod go.sum ./
RUN go mod download -x
# resets caches
COPY . .
ARG VER
RUN make build

FROM quay.io/prometheus/busybox:latest
COPY --from=builder /build/thanos-kit /bin/thanos-kit
ENTRYPOINT ["/bin/thanos-kit"]
