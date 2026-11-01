# What ./e2e.py needs, so `make e2e` never reaches the host's desktop, bus or display.
FROM golang:1.27.3-trixie
RUN apt-get update && apt-get install -y --no-install-recommends \
        mpv ffmpeg chromium dbus-daemon xvfb xauth python3 \
    && rm -rf /var/lib/apt/lists/*
