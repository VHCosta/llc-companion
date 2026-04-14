# llc-companion

Local compile-and-run helper for the [C Systems Lab](https://c-systems-lab.pages.dev) course.

Runs a tiny HTTP/WebSocket server on `127.0.0.1:27183`. The course app detects it, then routes `gcc` commands through your real compiler instead of showing canned output.

## What it does

- Receives lesson files + a shell command from the course terminal
- Writes those files to a temp directory and runs an allowlisted command chain
- Streams stdout/stderr back line-by-line over WebSocket
- 5-second timeout, 64 KB output cap, temp dir cleaned up after each run

## Install

### macOS / Linux

```bash
# Download (pick your platform):
curl -Lo llc-companion https://github.com/VHCosta/llc-companion/releases/latest/download/llc-companion-darwin-arm64
# or: llc-companion-darwin-amd64  (Intel Mac)
# or: llc-companion-linux-amd64

chmod +x llc-companion
./llc-companion
```

### Windows

Requires WSL2 with gcc installed. Run inside WSL2:

```bash
curl -Lo llc-companion https://github.com/VHCosta/llc-companion/releases/latest/download/llc-companion-linux-amd64
chmod +x llc-companion
./llc-companion
```

### Keep it running

Add to your shell profile or run it in a background terminal session. It starts in under a second and uses ~3 MB of memory.

```bash
# Verify it's running:
curl http://127.0.0.1:27183/ping
# → {"os":"darwin","status":"ok","version":"1.0.0"}
```

## Build from source

```bash
git clone https://github.com/VHCosta/llc-companion
cd llc-companion
go mod tidy
make dev          # current platform
make all          # all platforms → dist/
```

Requires Go 1.21+.

## Protocol

The course app speaks to the companion over WebSocket at `ws://127.0.0.1:27183/run`.

**Client → companion** (one message per run):
```json
{
  "files": [
    {
      "filename": "vec.h",
      "source": "#ifndef VEC_H\n#define VEC_H\n..."
    },
    {
      "filename": "vec.c",
      "source": "#include \"vec.h\"\n..."
    },
    {
      "filename": "test_vec.c",
      "source": "#include \"vec.h\"\nint main(void) { ... }"
    }
  ],
  "cmd":      "gcc -std=c11 -Wall vec.c test_vec.c -o vec_demo && ./vec_demo"
}
```

Legacy single-file payloads using `source` + `filename` are still accepted for compatibility.

**Companion → client** (streamed):
```json
{ "type": "out",     "text": "Hello, World!" }
{ "type": "err",     "text": "hello.c:3:5: error: ..." }
{ "type": "success", "text": "exit 0" }
{ "type": "exit",    "code": 0 }
```

`exit` is always the last message. `success` is sent only on exit code 0.

## Security

The companion only binds to `127.0.0.1` and only accepts browser requests from
approved origins. It does not execute arbitrary shell syntax: it only allows
`gcc`/`clang`/`cc` compile commands plus `./binary` execution in the temp workdir.
Materialized lesson files are restricted to `.c`, `.h`, and `.s`.

If you serve the course from another origin, set `LLC_ALLOWED_ORIGINS`:

```bash
LLC_ALLOWED_ORIGINS="https://example.com,http://localhost:4173" ./llc-companion
```
