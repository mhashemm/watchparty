# What ./e2e.py needs, so `make e2e` never reaches the host's desktop, bus or display.
# It runs as a user with the host's uid and gid, so the logs it leaves are the host
# user's; dbus-daemon refuses a uid that isn't in /etc/passwd.
FROM golang:1.27.4-trixie
# the GUI builds with cgo here (Gio's headers) and runs on Xvfb through Mesa; its real-window
# cases click with xdotool and read the clipboard with xclip
RUN apt-get update && apt-get install -y --no-install-recommends \
        mpv ffmpeg dbus-daemon xvfb python3 xdotool xclip \
        pkg-config libwayland-dev libx11-dev libx11-xcb-dev libxkbcommon-x11-dev libgles2-mesa-dev \
        libegl1-mesa-dev libffi-dev libxcursor-dev libvulkan-dev libgl1-mesa-dri \
    && rm -rf /var/lib/apt/lists/*
ARG UID=1000 GID=1000
RUN groupadd -o -g $GID e2e && useradd -o -m -u $UID -g $GID e2e && install -d -o $UID -g $GID /cache
USER e2e
ENV GOCACHE=/cache/go-build GOMODCACHE=/cache/mod GOFLAGS=-buildvcs=false CGO_ENABLED=0 E2E_CONTAINER=1
