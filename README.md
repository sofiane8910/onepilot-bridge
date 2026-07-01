# onepilot-bridge

A tiny persistent terminal daemon for your own server. It keeps your shell and
its scrollback alive between connections, so a mobile SSH client can disconnect,
come back later, and pick up the same session with full history.

Built for the [Onepilot](https://onepilotapp.com) iOS app, which installs and
updates it automatically when you enable persistent sessions. It is a standalone
tool: you can build, inspect, run, and remove it yourself.

## How it works

```
phone ──ssh──> your server ──127.0.0.1:port──> onepilot-bridge serve
                                                    └── owns persistent shells (PTY)
```

- `serve` starts a self detaching daemon that owns one or more shells.
- The daemon listens on a **loopback port only** (127.0.0.1). No inbound port is
  opened; clients reach it through the SSH port forward they already have.
- Each session keeps a scrollback ring in memory. On reattach the daemon replays
  history, then streams live output.

## Security and privacy

- **No outbound connections.** The binary never dials the internet: no
  telemetry, no update checks, no Onepilot servers. The only sockets it touches
  are `127.0.0.1` listeners and dials on the same machine.
- **No credentials.** It stores nothing under `~/.onepilot` except the binary,
  a port file, and the daemon itself; your SSH keys and passwords never pass
  through it.
- **No sudo.** Everything lives in your home directory and runs as your user.
- Static binary, single small dependency ([creack/pty](https://github.com/creack/pty)).

## Subcommands

```
onepilot-bridge serve [--port N]     start (or confirm) the daemon; prints the bound port
onepilot-bridge new [--session ID]   create a session and print its id
onepilot-bridge attach --session ID  connect stdin/stdout to a session
onepilot-bridge list                 print live session ids
onepilot-bridge kill --session ID    terminate a session
onepilot-bridge --version            print the version
```

## Install

The Onepilot app installs the right binary for your server automatically.
Manual install:

```sh
mkdir -p ~/.onepilot/bin
curl -fsSL -o ~/.onepilot/bin/onepilot-bridge \
  https://github.com/sofiane8910/onepilot-bridge/releases/latest/download/onepilot-bridge-$(uname -s | tr A-Z a-z)-$(uname -m | sed -e 's/x86_64/amd64/' -e 's/aarch64/arm64/')
chmod +x ~/.onepilot/bin/onepilot-bridge
~/.onepilot/bin/onepilot-bridge serve
```

Verify a download against `SHA256SUMS` from the same release.

## Build from source

```sh
go build ./cmd/onepilot-bridge          # current platform
scripts/build-release.sh                # all four release targets + SHA256SUMS
```

## Uninstall

```sh
pkill -f 'onepilot-bridge serve'; rm -rf ~/.onepilot
```

Running shells and their scrollback go with it.

## License

MIT
