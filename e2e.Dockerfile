# What ./e2e.py needs, so `make e2e` never reaches the host's desktop, bus or display.
# It runs as a user with the host's uid and gid, so the logs it leaves are the host
# user's; dbus-daemon refuses a uid that isn't in /etc/passwd.
FROM golang:1.27.3-trixie
RUN apt-get update && apt-get install -y --no-install-recommends \
        mpv ffmpeg chromium dbus-daemon xvfb xauth python3 \
    && rm -rf /var/lib/apt/lists/*
ARG UID=1000 GID=1000
RUN groupadd -o -g $GID e2e && useradd -o -m -u $UID -g $GID e2e && install -d -o $UID -g $GID /cache
USER e2e
ENV GOCACHE=/cache/go-build GOMODCACHE=/cache/mod GOFLAGS=-buildvcs=false E2E_NO_SANDBOX=1
