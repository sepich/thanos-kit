FROM quay.io/prometheus/busybox:latest
COPY ./thanos-kit /bin/thanos-kit
ENTRYPOINT [ "/bin/thanos-kit" ]
