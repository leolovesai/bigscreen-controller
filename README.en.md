# Corporate Big-Screen Web Controller

English · [中文](README.md)

Turn a phone into the remote for a corporate big screen. A single-binary service runs locally, Chrome displays your dashboards fullscreen, and a phone browser mirrors the screen live — tap, drag, swipe directly on it.

Built for meeting-room displays, on-call monitoring walls, and exhibition boards: nobody wants to walk over to the screen machine, but the operator always has a phone in hand.

No screenshots, no synthetic system events, no system permissions. Everything goes through the Chrome DevTools Protocol and only ever touches the page inside Chrome.

```
┌──────────────┐   HTTP/WS :7788    ┌────────────────┐   CDP(ws)   ┌────────┐
│   Phone      │ ◄───────────────► │ deskctl agent  │ ◄─────────► │ Chrome │
│ canvas+keys  │  JSON / JPEG      │  Go single bin │             └────────┘
└──────────────┘                    └────────────────┘
```

## Features

| Capability | Details |
|------------|---------|
| Scene switching | One URL per scene in the config; one tap opens it fullscreen on the big screen |
| Live screen | CDP screencast streaming — zero traffic and zero CPU while the page is static |
| Direct control | Tap, drag, and scroll on the mirror; it acts on the real page |
| Swipe gestures | The server synthesizes a 12-step drag, so a dropped connection never leaves the button stuck down |
| Clickable-element scan | Auto-lists every clickable element on the current page; tapping one equals clicking it |
| Window control | Enter/exit fullscreen, raise Chrome to front, restart Chrome |
| Mobile-first UI | Four layouts — phone portrait, phone landscape, tablet, desktop; 54px minimum hit targets |
| QR onboarding | When listening on 0.0.0.0, the startup log prints a LAN QR code that logs in without a password |
| Zero-dependency deploy | One binary with the frontend embedded; the target machine only needs Chrome |

## Quick Start

Requires Go 1.24+ and Google Chrome.

```bash
git clone https://github.com/leolovesai/bigscreen-controller.git
cd bigscreen-controller

# At minimum change the password, then point scenes at your own pages
cp config.example.yaml config.yaml

go build -o deskctl .
./deskctl
```

Startup output:

```
[deskctl] local  http://127.0.0.1:7788/?k=<token>
[deskctl] phone  http://192.168.x.x:7788/?k=<token>
         █▀▀▀▀▀█ ▄▀ █▀▀▀▀▀█     ← scan to open, already logged in
         █ ███ █ ▀▄ █ ███ █
```

Scan the code or open the LAN address on your phone. Chrome is not pre-warmed — it launches on the first scene tap (3–4s cold start).

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-config` | `config.yaml` | Config path; missing file falls back to defaults |
| `-addr` | empty | Override the listen address, e.g. `0.0.0.0:7788` |
| `-open` | false | Open the controller page after startup |
| `-v` | false | Print version, platform, and Chrome path |

## Configuration

```yaml
server:
  host: 0.0.0.0          # Reachable from the LAN; use 127.0.0.1 for local only
  port: 7788
  password: change-me    # Required when host is not loopback, otherwise startup fails
  autoOpen: false        # A server should not steal focus

capture:
  quality: 60            # JPEG quality; resolution follows the client panel width

chrome:
  binary: ""             # Empty = auto-detect
  userDataDir: ~/.deskctl/chrome-profile
  windowSize: [1512, 945]
  appMode: true          # No tab bar or address bar — pure web page when fullscreen
  extraArgs:
    - --disable-features=Translate,MediaRouter

scenes:
  - id: dashboard
    name: Dashboard
    url: https://example.com/dashboard
    fullscreen: true
    wait: load
    timeoutMs: 8000
```

Add a scene by appending a YAML block; restart to apply.

> Setting `server.host` to `0.0.0.0` exposes the console to everyone on the subnet. Change the password, or bind to `127.0.0.1` instead.

## Using the Controller

**Scenes** — one tap launches Chrome, navigates, and goes fullscreen according to the scene config. The real window state is pushed back to the top bar.

**Page buttons** — a new page is scanned automatically for clickable elements (links, buttons, `role=button`, `onclick` containers), ordered by document position. Tapping one clicks the real element. Buttons that appear from lazy loading need a manual refresh from the top-right icon.

**Screen area** — tap, drag, scroll. The frame is letterboxed to fit; taps landing in the margins are discarded.

**Bottom row** (always visible, highest frequency): swipe left / swipe right / page up / page down.

**Top bar icons**: stream settings, fullscreen, bring to front, restart Chrome, reconnect.

## How It Works

**Everything goes through CDP — no system APIs.** Frames come from `Page.startScreencast`, mouse input from `Input.dispatchMouseEvent`. The payoff: no Screen Recording or Accessibility permissions, `CGO_ENABLED=0` cross-compilation, and stray input can only ever reach Chrome, never the desktop.

**Streaming is change-driven.** Chrome emits a frame only when the page changes. The server never decodes, rescales, or re-encodes — it forwards. There is no frame-rate knob: a static page sends nothing. Resolution tracks the phone's panel width, so a 780px panel gets 780px frames at 8–34KB each.

**Gestures are synthesized server-side** as 12 steps of `pressed → moved(easeOut) → released`. Any mid-sequence failure is covered by a deferred `released`, so the page never stays stuck in a pressed state.

**Three layers of auth.** A 32-byte random token generated at startup (`?k=` for passwordless entry); a cookie planted on first visit so the URL can be cleaned; a mandatory password for LAN access. WebSocket handshakes validate that Origin and Host are same-origin to block CSRF from other web pages. Wrong passwords are rate-limited to 1/s per IP.

The cookie is derived from the password, so restarting the service keeps you signed in; changing the password invalidates every old session, and the controller answers a 401 by showing the password box instead of retrying forever.

Deeper implementation notes and the traps we hit — macOS silently ignoring the CDP fullscreen state, `captureScreenshot` downscaling causing screen flicker, app windows appearing late in the target list — are documented in [DESIGN.md](DESIGN.md).

## Debug Tools

Four standalone probes under `cmd/`, all running the production code paths:

```bash
go run ./cmd/probe                            # Smoke test: connect, run scene, receive frames
go run ./cmd/screencast                       # Verify frame size, static dedup, panel-width switching
go run ./cmd/uiprobe                          # Validate element scanning and clicking
go run ./cmd/verify -k <token> -scene <id>    # Sample window luminance to detect display-surface churn
```

`verify` is the regression guard for the streaming approach: a luminance range well off baseline means the downscale path is touching the live display surface again.

## Known Limitations

| Item | Notes |
|------|-------|
| Chrome only | No other desktop window receives input — that is the design boundary |
| macOS fullscreen | Chrome ignores the CDP `fullscreen` state and lands on `maximized`. It looks edge-to-edge, but is not a separate fullscreen space at the OS level |
| No keyboard input | Mouse, wheel, and gestures only; text entry is not supported yet |
| Element scan | Capped at 60 entries per page, main document only — elements inside iframes are invisible to it |
| Dynamic content | Newly rendered buttons from scrolling or async updates do not appear automatically |
| Frame rate | Driven by page changes; there is no fixed-FPS mode |
| Bot detection | Some sites (search result pages, for instance) challenge CDP clicks with a CAPTCHA. Manual clicks trigger it too |

## Project Layout

```
bigscreen-controller/
├── main.go                    # Startup, embedded frontend, graceful shutdown, terminal QR
├── internal/
│   ├── config/config.go       # YAML loading and validation
│   ├── chrome/
│   │   ├── cdp.go             # Minimal CDP client
│   │   ├── elements.go        # Clickable-element scan and click
│   │   └── manager.go         # Process, navigation, fullscreen, streaming, mouse
│   └── server/
│       ├── server.go          # Routing, token/cookie auth
│       └── ws.go              # WS protocol, frame relay, gesture synthesis
├── web/                       # Controller frontend (vanilla JS, no build step)
├── cmd/                       # Four debug probes
├── docs/                      # UI screenshots
├── config.example.yaml
└── DESIGN.md                  # Architecture and implementation notes
```

## Dependencies

- [gorilla/websocket](https://github.com/gorilla/websocket) — WebSocket
- [skip2/go-qrcode](https://github.com/skip2/go-qrcode) — Terminal LAN QR code
- [gopkg.in/yaml.v3](https://gopkg.in/yaml.v3) — Config parsing

The frontend uses no framework and no build step; icons are inline SVG.

## License

[MIT](LICENSE) © 2026 leo
