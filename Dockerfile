FROM golang:alpine as builder
WORKDIR /build
RUN apk add --no-cache make git
# try to cache deps
COPY go.mod go.sum ./
RUN go mod download -x
# resets caches
COPY . .
RUN make test && make build

FROM quay.io/prometheus/busybox:latest
COPY --from=builder /build/thanos-kit /bin/thanos-kit
ENTRYPOINT [ "/bin/thanos-kit" ]
