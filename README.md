# utorr

A secure, fast, multi-threaded torrent downloader with resume capabilities.

## Installation

Ensure you have Go 1.24+ installed.

```bash
# Fetch dependencies
go mod tidy

# Build the project
make
```

## Usage

Use the compiled binary for the best performance (avoids compilation delay):

```bash
./builds/utorr -o downloads "magnet:?xt=urn:btih:..."
```

### Options

- `-o <dir>`: Output directory (default: `downloads`)
- `-session <dir>`: Session data directory (default: `session`)
- `-max-conns <n>`: Max peer connections (default: 80)
- `-seed`: Seed after completion
- `-disable-utp`: Disable uTP
- `-disable-ipv6`: Disable IPv6

### Interactive Commands

During download:
- `p`: Toggle pause/resume
- `q`: Quit gracefully (state is saved)

## Why is `go run .` slow?

`go run` compiles the entire project and its many dependencies (including WebRTC and DHT stacks) every time it is executed. For the fastest experience, especially when just viewing help info, always use the compiled binary in the `builds/` directory.
