# watchparty

Watch the same video with friends, each on your own computer, in [mpv](https://mpv.io).
When anyone pauses, resumes or seeks, everyone's player follows. There is no host:
any of you can drive.

Everyone needs their own copy of the same file, or the same URL. Nothing is streamed
between you. Only pause and seek events are sent, so a slow connection is fine.

## Install

1. Install mpv (`apt install mpv`, `brew install mpv`, or [mpv.io](https://mpv.io/installation/)).
2. Download a binary from [Releases](https://github.com/mhashemm/watchparty/releases):

   | | App | Command line |
   |---|---|---|
   | Linux x86-64 / ARM64 | `watchparty-gui_amd64` / `_arm64` | `watchparty_amd64` / `_arm64` |
   | macOS Intel / Apple silicon | `watchparty-gui_mac_amd64` / `_mac_arm64` | `watchparty_mac_amd64` / `_mac_arm64` |
   | Windows | `watchparty-gui.exe` | `watchparty.exe` |

   On Linux and macOS, `chmod +x` it first. The app updates itself from Settings.
   Windows builds are provided but haven't been tested on Windows yet.

## The app

Run `watchparty-gui`. It opens a small window.

1. Pick your video.
2. Choose **Over the internet** (through a relay) or **Same Wi-Fi**.
3. The first person leaves the room and secret empty and presses **Start the party**.
   An invite appears. Send it to your friends.
4. Friends paste the invite into **Got an invite?**, pick the same video, and start.

mpv opens for everyone. Pause, seek and resume in mpv as usual.

## The command line

Each command prints `your address to share is …`. Pass those flags to your friends.

Same network:

```sh
watchparty -local -file movie.mkv                       # prints -addrs 192.168.1.5:6969 -secret XYZ
watchparty -local -file movie.mkv -port 6970 -addrs 192.168.1.5:6969 -secret XYZ
```

Over the internet, without `-local`, the program opens a port on your router through
UPnP. Your router has to support UPnP for this to work.

Through a relay, which works behind any NAT:

```sh
watchparty -relay wss://relay.example.com -file movie.mkv                         # prints -room … -secret …
watchparty -relay wss://relay.example.com -room ROOM -secret SECRET -file movie.mkv
```

`watchparty -h` lists every flag. `-mpvFlags` passes extra options to mpv.

## How it stays in sync

Playback is never corrected while it runs. Instead, anything that would put people
out of step pauses everyone, and someone resumes by hand:

- a seek while playing
- someone's player running out of buffered video
- someone joining mid-way, who then starts at the party's position

Every pause also carries the position where it stopped, so network delay can't leave
people apart.

A joiner whose file differs from the party's is refused. Files are compared by a
fingerprint of their contents, so a renamed copy still matches. A different encode
or cut does not.

## Running a relay

```sh
docker build -f relay/Dockerfile -t watchparty-relay .
docker run -p 80:80 -e CREATE_KEY=somekey watchparty-relay
```

With `CREATE_KEY` set, only someone passing `-createKey somekey` (in the app, under
Settings) can open a room. Guests don't need it. Put it behind a TLS proxy and use
`wss://`. `relay/CLAUDE.md` lists every setting and abuse limit.

## Building

Needs Go (the version in `go.mod`).

```sh
make -j          # the CLI, the app and the relay, into .bin/
make -j test     # unit tests and the end-to-end suite (needs mpv and ffmpeg)
```

`CLAUDE.md` describes the architecture.
