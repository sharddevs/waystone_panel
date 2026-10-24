<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# Deployment files

## Public browser access

Browsers never talk to the controller directly. They reach it over HTTPS
through a reverse proxy, which forwards to the controller's private
socket `/run/waystone-controller/web.sock`. In v0.1 the supported proxy
is **Caddy**, configured exactly as in [`caddy/Caddyfile.example`](caddy/Caddyfile.example).

What the controller enforces on every browser request:

- It only accepts connections on `web.sock` from one uid, the proxy's
  (`WAYSTONE_PROXY_UID`), checked with the kernel. The socket itself is
  `root:waystone-web 0660`, and only the proxy's user is in `waystone-web`.
- The request must name the canonical host from `WAYSTONE_PUBLIC_URL`
  (anything else gets 421), and carry exactly the `X-Forwarded-For`,
  `X-Forwarded-Proto` and `X-Forwarded-Host` headers the Caddyfile sets.
  Any of `Forwarded`, `X-Real-IP` and the other headers the Caddyfile
  removes makes the request fail (400), because seeing one means the
  proxy isn't configured as supported.
- State-changing requests must come from the canonical origin.
- Every response carries a strict Content-Security-Policy and the other
  browser security headers; HTTPS responses carry HSTS.

### Setting it up

1. Pick the public URL, e.g. `https://panel.example.com`. It must be
   https, with no path. Put it in `/etc/waystone/controller.env` as
   `WAYSTONE_PUBLIC_URL`.
2. Install Caddy, and give it access to the socket:

   ```
   sudo apt-get install caddy
   sudo groupadd --system waystone-web
   sudo usermod -aG waystone-web caddy
   ```

3. Put `id -u caddy` in `controller.env` as `WAYSTONE_PROXY_UID`.
4. Copy `caddy/Caddyfile.example` to `/etc/caddy/Caddyfile` and replace
   `panel.example.com` with your host. Caddy gets a certificate for it
   automatically if the name is public and ports 80 and 443 reach it;
   otherwise add a `tls <cert> <key>` line with your own certificate.
   If you also add a `bind` line, Caddy 2.6 puts its automatic
   http-to-https redirect on only one of the addresses; add an
   `http://<host>` block with the same `bind` and a `redir` to fix that.
5. Install `systemd/waystone-controller-web.socket` with the other units,
   then:

   ```
   sudo systemctl daemon-reload
   sudo systemctl stop waystone-controller.service
   sudo systemctl enable --now waystone-controller-web.socket
   sudo systemctl start waystone-controller.service
   sudo systemctl restart caddy.service
   ```

   Stop the controller first: systemd won't start a new socket for a
   service that is already running.

6. Check it: `curl -i https://panel.example.com/healthz` answers
   `{"status":"ok"}` with a `Strict-Transport-Security: max-age=300`
   header.

HSTS starts at 300 seconds so a mistake is cheap while you validate the
setup. Once HTTPS works, raise `WAYSTONE_HSTS_MAX_AGE` (at most two
years). Waystone never sends `includeSubDomains` or `preload`.

Don't add `trusted_proxies` to the Caddyfile, and don't put another proxy
or CDN in front of Caddy: v0.1 supports exactly one proxy, facing browsers
directly.

## Building the controller with the web UI

The browser UI lives in `web/` (Vite, React, TypeScript). A release
controller embeds it, so the build needs Node.js once, at build time:
the version in `web/.nvmrc`, from the official tarball (not the
distribution's `nodejs` package).

```
cd web
npm ci                 # exactly package-lock.json; install scripts never run (.npmrc)
npm test               # unit tests, including the AA contrast check
npm run build          # type-checks, then writes internal/webui/dist/
cd ..
go build -tags ui -o waystone-controller ./cmd/controller
```

`internal/webui/dist/` is never committed. `go build` without `-tags ui`
still works (CI, the Windows cross-build) and gives a controller that
serves a plain "web UI isn't built" page instead of the UI; its startup
log says `web UI: not built into this binary`. Built with `-tags ui`, the
log says `web UI: embedded`, and a build whose `index.html` is missing or
breaks the Content-Security-Policy refuses to start.

Files under `/assets/` have hashed names and are cached for a year;
every page is `index.html`, never cached, so a reload of any address in
the UI works.

### Developing the UI

Build the controller with `-tags devweb` and start it with
`-dev-ui-dir <dir>`, where `<dir>` holds a web build. It serves the UI
from disk and reads it again on every request, so
`npm run build -- --watch --outDir <dir>` in `web/` shows up on the next
reload, behind the real Caddy, CSP, guard and cookie. The controller's
user must be able to read `<dir>`. A release build refuses
`-dev-ui-dir`.
