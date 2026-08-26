# Netwrck Agent Bridge

This adds a new kitten command:

```bash
kitten netwrck-agent doctor
kitten netwrck-agent serve --listen-addr 127.0.0.1:8420
```

The bridge is designed for the "one input box, two execution paths" UX:

- obvious shell goes to the shell lane
- obvious natural language goes to the agent lane
- borderline inputs go through `gobed` phrase embedding matching
- if the first command token is not executable on the local machine, the router falls back to the agent lane

## Local discovery

`codex` is discovered in this order:

1. `--codex-bin`
2. `CODEX_BIN`
3. `../codex/target/debug/codex`
4. `../codex/codex-rs/target/debug/codex`
5. `/home/lee/code/codex/target/debug/codex`
6. `/home/lee/code/codex/codex-rs/target/debug/codex`
7. `codex` on `PATH`

`gobed` is wired through a local Go module replace:

```go
replace github.com/lee101/gobed => ../gobed
```

No symlink is required as long as `../gobed` exists.

## HTTP API

### `GET /healthz`

Returns a basic liveness response.

### `POST /route`

```json
{"input":"make test"}
```

Response:

```json
{
  "lane": "ambiguous",
  "reason": "gobed scores were close ...",
  "confidence": 0.04,
  "run_as_choices": ["shell", "agent"]
}
```

### `POST /shell`

```json
{"command":"git status","cwd":"/home/lee/code/kitty"}
```

### `POST /agent`

```json
{"prompt":"explain this repo","cwd":"/home/lee/code/kitty"}
```

The agent endpoint starts or reuses a Codex app-server thread and returns the final assistant response text.
