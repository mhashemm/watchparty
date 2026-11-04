# watchparty

Watch the same video with friends, each on your own computer, in [mpv](https://mpv.io).
When anyone pauses, resumes or seeks, everyone's player follows. There is no host:
any of you can drive.

Everyone needs their own copy of the same file, or the same URL. Nothing is streamed
between you. Only pause and seek events are sent, so a slow connection is fine.

## Install

1. Install mpv (`apt install mpv`, `brew install mpv`, or [mpv.io](https://mpv.io/installation/)).
2. Install the app:

   - **Linux and macOS:** `curl -fsSL https://raw.githubusercontent.com/mhashemm/watchparty/master/install.sh | sh`.
     On Linux it lands in `~/.local/bin` with an entry in your app menu; on macOS it is
     `~/Applications/Watchparty.app`.
   - **Windows:** run `watchparty-gui-setup.exe` from [Releases](https://github.com/mhashemm/watchparty/releases).
     It installs for your user only, with Start menu and desktop shortcuts.

   The app updates itself from Settings.

Or download a binary from [Releases](https://github.com/mhashemm/watchparty/releases)
(`chmod +x` it first on Linux and macOS):

| | App | Command line |
|---|---|---|
| Linux x86-64 / ARM64 | `watchparty-gui_amd64` / `_arm64` | `watchparty_amd64` / `_arm64` |
| macOS Intel / Apple silicon | `watchparty-gui_mac_amd64` / `_mac_arm64` | `watchparty_mac_amd64` / `_mac_arm64` |
| Windows | `watchparty-gui.exe` | `watchparty.exe` |

## The app

Run `watchparty-gui`. It opens a small window.

1. Pick your video, a `.torrent` file or a magnet link. To watch a season from a video, tick
   **Also load the other videos in this folder**.
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
UPnP. Your router has to support UPnP for this to work, and your connection needs a
public address: behind carrier NAT, common on mobile routers, it warns you to use a relay.

Through a relay, which works behind any NAT:

```sh
watchparty -relay wss://relay.example.com -file movie.mkv                         # prints -room … -secret …
watchparty -relay wss://relay.example.com -room ROOM -secret SECRET -file movie.mkv
```

`watchparty -h` lists every flag. `-mpvFlags` passes extra options to mpv. `-update`
installs the latest release over the binary, and `-version` prints the one you have.

To watch a season, add `-playlist`. It starts on `-file` and loads the other videos in its
folder, so back and next in mpv go to the episodes either side of it. Everyone needs the
same videos in the same order, and `-playlist` too; names can differ.

## How it stays in sync

Playback is never corrected while it runs. Instead, anything that would put people
out of step pauses everyone, and someone resumes by hand:

- a seek while playing
- someone's player running out of buffered video
- someone joining mid-way, who then starts at the party's position
- moving to another episode, by hand or when one ends, which stops everyone at its start

Every pause also carries the position where it stopped, so network delay can't leave
people apart.

`-file` can be a `.torrent` file or a magnet link. Everyone uses the same one. It plays
while it downloads, a pack of episodes plays as a playlist with only the current one
downloading, and running out of downloaded video pauses the party. What you watched stays in your cache folder until you leave, when it is deleted, so make
sure the disk has room for it. As with any torrent, peers see your IP address, and you
upload to them while you watch.

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
