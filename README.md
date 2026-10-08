# AD_EXPLORER

A self-hosted Web gateway for RDP sessions. A browser talks to a Go process over
one WebSocket per session; the Go process drives a
[grdp](https://github.com/adfnekc/grdp) client on the other side.

Host addresses and passwords live **in the browser**. The gateway holds a
password for the length of one session, never writes it anywhere, and never logs
it. There is no database and no key to manage.

## Try it

```sh
go build -o ad_explorer ./cmd/ad_explorer

# A synthetic desktop that draws the input it receives. No host is contacted.
./ad_explorer -demo -password=secret -listen 127.0.0.1:8443 -secure-cookie=false
# → http://127.0.0.1:8443/
```

Then add a host in the left panel and connect. Sketch out what works and what
does not:

| | |
| --- | --- |
| Works | sign-in, host list, session lifecycle, the desktop picture, pointer shapes, keyboard and mouse, composer input such as Chinese, clipboard log, **file transfer both ways**, live desktop resizing, quality control, full screen |
| Waiting on grdp | nothing |

The demo desktop also has a working file transfer: share a file and it appears
as one the remote offered, so downloading it runs the full round trip — offer,
manifest, range request, chunked download — without a remote host.

Where something is missing, the gateway names the gap in the UI rather than
showing an empty canvas, so nothing looks like it works when it does not.

For a real host, drop `-demo` and make sure the cookie flag matches the scheme
you are serving on.

## Security posture

**Two different passwords, and it matters which is which.**

| | Where it lives | What it protects |
| --- | --- | --- |
| Console password | the gateway process (`-password-file`, `-password`, `ADX_PASSWORD`) | access to the gateway itself |
| Host passwords | the browser's `localStorage` | the remote machines |

The gateway never receives the second one except in the request that opens a
session, and it keeps it only for that session.

What that buys and what it does not:

- No credential store on the server, so there is nothing to dump.
- No server-side session state, so a restart logs everyone out and surprises
  nobody.
- Passwords travel in request bodies, so **the gateway must be served over
  HTTPS**. `-secure-cookie=false` exists for a loopback interface and warns when
  used.
- The host passwords in `localStorage` are **not encrypted**. Anything that can
  run script in the page can read them. That is the trade being made: the
  browser is the trust boundary, and the defence is the strict Content Security
  Policy the gateway sends.

grdp used to write the account password to its log in plaintext on every NLA
connection. That is fixed as of grdp v0.8.1, which also added a test that fails
its build if a password field is handed to a log call again. `internal/rdp`
still pins the log level at WARN, now for the ordinary reason rather than as a
mitigation, and `docs/grdp-requirements.md` records the history.

On this side, `-password` on the command line is visible through `ps(1)`. The
gateway warns and suggests `-password-file`, which refuses a file that is group
or world readable.

There is a test that drives a real session against an unreachable host and
asserts the password appears in neither the log nor the response body.

## File transfer

Both directions, and the gateway stores neither:

- **To the remote.** The browser offers files; the gateway installs a clipboard
  provider that advertises them. When a paste on the remote asks for a range,
  the gateway asks the browser for exactly those bytes. A four gigabyte file
  never has to fit in memory anywhere.
- **From the remote.** The remote's clipboard manifest appears in the panel.
  Downloading streams the file back in chunks, reassembled in the browser.

The chunks are binary frames carrying their own offset, so a chunk is placed by
what it says rather than by the order it arrived in.

## Performance

Encoding is the expensive part of a session, so it is measured rather than
guessed at. On a Ryzen 7 7840HS, one 1280x720 JPEG frame costs **32 ms** and a
400x250 region **3.7 ms**, which is roughly 30 ns per pixel. Everything around
it is noise: merging sixty-four dirty regions costs 28 µs.

That is why a session does not encode once per update from the server. It holds
the newest frame and encodes it on a tick — twenty a second by default,
`-max-fps` to change it — with every region that arrived in between merged in.
A burst of forty updates becomes three encodes, and the picture a client draws
is always the desktop as of one moment rather than a mixture of two.

Go's JPEG encoder already reads `*image.RGBA` pixels directly, so there is no
per-pixel interface dispatch to remove; going faster would mean a different
codec, and WebP would mean cgo or a slower pure Go encoder.

## Layout

```
cmd/ad_explorer/        flags, startup warnings, graceful shutdown
internal/auth/          console password, HMAC session tokens, login rate limit
internal/wsframe/       20-byte binary frame header and the JSON control frames
internal/rdp/           the grdp adapter: connection, input, clipboard, errors
internal/stream/        dirty-region merging, adaptive quality, encoding
internal/session/       registry, event loop, frame pipeline, file transfer, demo
internal/httpx/         cookie auth, origin checks, CSP, REST, WebSocket upgrade
internal/webui/dist/    the frontend, embedded into the binary
prototype/              the Win32 Classic style sample the UI follows
docs/                   design spec and the grdp requirements list
```

## Development

```sh
go test ./... -race          # everything, including the concurrency paths
go test ./internal/wsframe -fuzz=FuzzParseFrame -fuzztime=30s
```

The interesting tests are the ones that would have hidden a bug: the frame
codec's fuzz corpus, the session tests that drive a fake transport through the
real control codec, and the log-redaction test.

`docs/superpowers/specs/` holds the design the code follows, including the
decisions that were taken deliberately rather than by default.
`docs/grdp-requirements.md` is the list of changes grdp needs, with the exact
API, semantics and acceptance for each.
