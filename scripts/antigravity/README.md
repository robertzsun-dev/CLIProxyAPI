Antigravity 1.5.14 rejects CLIProxyAPI's SSE heartbeat comments. This loopback
proxy removes those comments from Gemini streams. The wrapper reads the client
API key from `~/.config/cliproxy-clients/key` and launches the existing
Antigravity binary at `~/.local/lib/antigravity-cli/agy`.

From the repository root, set `CLIPROXY_UPSTREAM` to the URL your Antigravity
wrapper previously used for `GOOGLE_GEMINI_BASE_URL`, then run:

```sh
mkdir -p ~/.local/lib/antigravity-stream ~/.config/cliproxy-clients
printf 'CLIPROXY_UPSTREAM=%s\n' "${CLIPROXY_UPSTREAM:?set the CLIProxyAPI URL}" > ~/.config/cliproxy-clients/antigravity-stream.env
go build -o ~/.local/lib/antigravity-stream/antigravity-stream ./cmd/antigravity-stream
install -Dm644 scripts/systemd/antigravity-stream.service ~/.config/systemd/user/antigravity-stream.service
systemctl --user daemon-reload
systemctl --user enable --now antigravity-stream
install -Dm755 scripts/antigravity/agy ~/.local/bin/agy
```

Point T3's Antigravity binary path at `~/.local/bin/agy`.
