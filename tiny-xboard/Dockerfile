# Runtime image only: the DEVELOPER machine builds binaries (build.sh);
# the repository serves them as plain files; this image just runs them.
# No Go, no Git, no compilation anywhere in this image.

ARG TARGETARCH=amd64
FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY bin/tiny-xboard-linux-${TARGETARCH} /usr/local/bin/tiny-xboard
ENV GOMEMLIMIT=32MiB GOGC=70 GOMAXPROCS=1
VOLUME ["/etc/tiny-xboard"]
EXPOSE 8080
ENTRYPOINT ["tiny-xboard"]
# 0.0.0.0 so published ports work; use 127.0.0.1 when running with host networking.
CMD ["-dir", "/etc/tiny-xboard", "-listen", "0.0.0.0:8080"]