FROM golang:alpine as builder
WORKDIR /build
# try to cache deps
COPY go.mod go.sum ./
RUN go mod download -x
# resets caches
COPY . .
RUN CGO_ENABLED=0 go build -ldflags '-w -s'

FROM quay.io/prometheus/busybox:latest
COPY --from=builder /build/thanos-kit /bin/thanos-kit
ENTRYPOINT [ "/bin/thanos-kit" ]
