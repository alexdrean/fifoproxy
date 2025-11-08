# Async HTTP Proxy

Small, in-memory HTTP proxy that immediately returns `200 OK` to clients and forwards requests (method, headers, body, path & query) to a configured target domain asynchronously.

Key behavior

* Immediately responds `200 Accepted` when a request is enqueued.
* FIFO single-worker queue (in-memory).
* Retries delivery every **5s** on failure.
* If a message fails **more than 3 times**, sends a Telegram bot alert (if configured).
* Graceful shutdown: `Server.Shutdown` + worker drains queue before exit.
* Health endpoint: `GET /healthz` → `200 OK`.

---

## Compilation

Build the binary from source (assumes `main.go` in current directory):

```bash
go build -o proxy main.go
```

This produces the executable `./proxy`.

---

## Command-line parameters

All options are passed as CLI flags:

```
-target-url string          # REQUIRED: Base target URL (scheme+host, e.g. https://destination.example.com)
-port int                   # HTTP listen port (default: 8080)
-queue-size int             # Max in-memory queue size (default: 1000)
-max-retries int            # Max retries per job; 0 = infinite retries (default: 0)
-telegram-bot-token string  # Telegram bot token (optional)
-telegram-chat-id string    # Telegram chat ID (optional)
```

Notes:

* `-target-url` is required and should include scheme (http/https) and host. The proxy will append the original request path and query to this base URL.
* If `-max-retries` is `0` the proxy will retry indefinitely (still fires Telegram alert after >3 attempts).
* Telegram parameters are optional; if provided together the app will send a minimal JSON alert after >3 failures.

---

## Runtime behavior summary

* Immediately returns `200` and body `accepted` when a request is enqueued.
* If the in-memory queue is full, returns `503 Service Unavailable`.
* Forwards requests to: `TARGET_BASE + original_path + ?original_query`.
* Retries every 5s on any delivery error or non-2xx response.
* Uses a per-attempt timeout (5s).
* On SIGINT/SIGTERM the server stops accepting new requests, waits for in-flight handlers, then drains the queue before exit.

---

## Examples

Run directly:

```bash
./proxy \
  -target-url "https://destination.example.com" \
  -port 8080 \
  -queue-size 1000 \
  -max-retries 0 \
  -telegram-bot-token "123456:ABC-DEF..." \
  -telegram-chat-id "123456789"
```

Minimal (no Telegram):

```bash
./proxy -target-url "https://destination.example.com"
```

Health check:

```bash
curl -i http://localhost:8080/healthz
# HTTP/1.1 200 OK
# ok
```

---

## Running with PM2

1. Build the binary (see **Compilation**).

2. Start with PM2 and pass CLI args after `--`:

```bash
pm2 start ./proxy --name async-proxy -- \
  -target-url https://destination.example.com \
  -port 8080 \
  -queue-size 1000 \
  -max-retries 0 \
  -telegram-bot-token 123456:ABC-DEF \
  -telegram-chat-id 123456789
```

3. (Optional) Use an `ecosystem.config.js` for persistent configuration:

```js
module.exports = {
  apps: [
    {
      name: "async-proxy",
      script: "./proxy",
      args: [
        "-target-url", "https://destination.example.com",
        "-port", "8080",
        "-queue-size", "1000",
        "-max-retries", "0",
        "-telegram-bot-token", "123456:ABC-DEF",
        "-telegram-chat-id", "123456789"
      ],
      autorestart: true,
      restart_delay: 5000,
      watch: false
    }
  ]
};
```

Start with:

```bash
pm2 start ecosystem.config.js
pm2 save           # save process list (so pm2 startup can resurrect)
pm2 logs async-proxy
```

Useful PM2 commands:

```bash
pm2 list
pm2 logs async-proxy
pm2 restart async-proxy
pm2 stop async-proxy
pm2 delete async-proxy
pm2 startup        # generate startup script for system boot
```

---

## Troubleshooting & tips

* Ensure `-target-url` includes scheme (`http://` or `https://`) and host.
* If you enable Telegram alerts, the bot must be able to message the configured chat ID (use numeric chat ID or `@channelusername` as appropriate).
* Tune `-queue-size` to avoid OOM or frequent `503` under expected peak load.
* Use `pm2 logs` to inspect stdout/stderr logs from the process.
* For production, consider running behind a process supervisor (systemd) or containerizing the binary.
