# syntax=docker/dockerfile:1

# --- build ---
# Must satisfy the go directive in go.mod (1.25.0).
FROM golang:1.25-alpine AS build

WORKDIR /src

# Dependencies first, so editing source does not re-download the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Static binary: the final stage has no libc.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/sshplace .

# Staged so the runtime stage can copy it in with the right owner. distroless
# has no shell, so there is no chance to chown after the fact — and a /data that
# only root can write means host key generation fails on first boot.
RUN mkdir -p /out/data

# --- runtime ---
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/sshplace /usr/local/bin/sshplace

# 65532 is distroless's nonroot uid/gid. Numeric, because the id has to resolve
# without relying on the base image's /etc/passwd.
COPY --from=build --chown=65532:65532 /out/data /data

# The host key, canvas snapshot and event log all live here. Mount a volume on
# it: without one, restarting the container hands every client a new host key and
# they all see a man-in-the-middle warning.
VOLUME /data

USER 65532:65532

EXPOSE 2222 8080

ENV SSHPLACE_SSH_ADDR=:2222 \
    SSHPLACE_HTTP_ADDR=:8080 \
    SSHPLACE_DATA=/data

ENTRYPOINT ["/usr/local/bin/sshplace"]
