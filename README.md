# Gyemoim

Gyemoim is a local OpenAI Responses API gateway with a browser interface for provider connections, model routes, harness accounts, and request history. It is one executable with the WebUI embedded; it does not need a config file, installer, service manager, or separate frontend build.

## Requirements

- Linux or macOS on amd64 or arm64.
- Go 1.25 or newer to build from source.
- OpenAI Sign in with ChatGPT access for provider inference. A connected account must grant direct inference access.

The release build has CGO disabled. It does not need a C compiler or separate runtime assets.

## Build

From the repository root, build for the current machine:

~~~sh
CGO_ENABLED=0 go build -trimpath -o gyemoim ./cmd/gyemoim
~~~

To build all four release targets:

~~~sh
./scripts/build-release.sh
~~~

The script writes these executable files into dist:

- gyemoim-linux-amd64
- gyemoim-linux-arm64
- gyemoim-darwin-amd64
- gyemoim-darwin-arm64

Pass an output directory to use another location. A relative path is resolved from the directory where you run the script.

~~~sh
./scripts/build-release.sh "$HOME/Downloads/gyemoim-release"
~~~

The script creates that directory if needed, replaces only regular files with those four exact names, and leaves other files in it alone. It refuses symlinks and non-file targets. It does not run tests or install anything.

## Start Gyemoim

Run the binary for your operating system and processor. For example, on Linux amd64:

~~~sh
./dist/gyemoim-linux-amd64
~~~

Gyemoim listens only on 127.0.0.1, on port 9092 by default. Open http://127.0.0.1:9092/ in a browser. The loopback WebUI is available without an administrator account.

To choose another port, pass --port:

~~~sh
./dist/gyemoim-linux-amd64 --port 9093
~~~

The only command-line option is --port. Gyemoim creates its data directory at startup and prints the selected data path. Only one process can use a data directory at a time. A port already in use or a second process using the same data directory produces a startup error.

Press Ctrl-C to stop. SIGTERM also requests a graceful shutdown. The server waits up to 30 seconds for requests to finish, then cancels remaining work. Restart by running the same command again. Keep the port configured in your harness. The OAuth callback uses the active listener port for each sign-in; OpenAI's official Sign in with ChatGPT flow allows the loopback port to vary on later sign-ins while the scheme, host, and callback path stay the same. The authenticated changed-port flow has not been verified with a live account ([official registration and sign-in guidance](https://developers.openai.com/siwc/token-sharing-open-source/sign-in)).

## First-time setup

1. Open the Providers page, add an OpenAI provider, and choose Connect OpenAI account. Complete Sign in with ChatGPT in the browser. A linked account without the required direct-use permission cannot make inference requests.
2. On the Models page, create a local model name (an alias) and connect it to that provider and an exact upstream model ID. You can load the selected account's visible catalog; the upstream ID remains editable. Add optional Pi metadata only from a source you have verified.
3. On the Service accounts page, create or select a service account and explicitly grant it the model alias. New models are not granted automatically.
4. Issue a local API key. Gyemoim shows the plaintext once. Copy it then; it cannot be retrieved later. The key is for the harness and is separate from the OpenAI OAuth credentials.
5. For pi agent 1.0.0, open Pi agent setup for that account and select Check Pi setup. The UI lists every granted model and explains incomplete metadata. It generates a config fragment only when at least one granted reasoning model has complete metadata. Copy or download the fragment, set the shown environment-variable reference to the one-time key, and manually merge the provider entry into ~/.pi/agent/models.json. Preserve the file's existing providers and settings; Gyemoim does not edit it.

The Pi setup page requires exact model metadata: positive contextWindow and maxTokens, an input list that includes text, reasoning set to true, and a nonempty supportedReasoningEfforts list. It does not guess model capabilities. Generated config uses the exact Pi 1.0.0 thinkingLevelMap field and maps only explicitly supported reasoning levels. See [Pi setup](docs/pi.md) for the full details.

Gyemoim currently supports OpenAI through Sign in with ChatGPT. OpenAI API-key authentication and other provider types are not implemented. There is no CLI login flow; connect the account in the local WebUI.

## Call the local API

Use the model alias created in the WebUI. The local service account key authenticates these requests; OpenAI credentials stay in Gyemoim.

~~~sh
curl http://127.0.0.1:9092/v1/models \
  -H "Authorization: Bearer $GYEMOIM_API_KEY"
~~~

A non-streaming Responses request:

~~~sh
curl http://127.0.0.1:9092/v1/responses \
  -H "Authorization: Bearer $GYEMOIM_API_KEY" \
  -H "Content-Type: application/json" \
  --data '{"model":"my-model-alias","input":[{"role":"user","content":[{"type":"input_text","text":"Say hello in one sentence."}]}],"stream":false}'
~~~

For a streaming response, set stream to true and use curl's -N option to display events as they arrive. Keep the bearer value in an environment variable instead of putting a real key in command history.

The API exposes GET /v1/models and POST /v1/responses. Each model alias routes to one configured provider and upstream model. The first version has no automatic retries or fallback. OpenAI requests always go upstream with stream=true and store=false; non-streaming client requests are collected and returned as JSON. Unsupported request capabilities produce errors instead of being silently removed. See [the Responses contract](docs/responses-contract.md).

## Networking

Gyemoim binds to the loopback address and is intended for local use. If a harness uses an HTTP proxy, configure its proxy bypass for 127.0.0.1 and localhost (often through NO_PROXY) so calls reach Gyemoim directly.

Gyemoim's outbound OpenAI OAuth, model-catalog, and Responses transports do not use HTTP_PROXY or HTTPS_PROXY. A proxy configured for the harness does not change this behavior.

## Data, privacy, and backups

The data directory is created automatically with owner-only access:

- Linux: XDG_DATA_HOME/gyemoim when XDG_DATA_HOME is an absolute path; otherwise ~/.local/share/gyemoim.
- macOS: ~/Library/Application Support/Gyemoim.

The directory is mode 0700 and its data files are mode 0600. SQLite config.db holds provider OAuth credentials, provider registrations, service-account metadata and key hashes, model routes, and grants. OAuth credentials are ordinary SQLite data protected by filesystem permissions; SQLite and request history are not encrypted by the application. The one-time plaintext service-account key is not stored.

Request history is stored separately under history/ as NDJSON and compressed closed segments. Gyemoim records full incoming and effective request bodies, responses, tool definitions, and tool results. It excludes gateway-managed credentials, but it does not mask arbitrary content inside prompts or tool results. Recorded content can include secrets you supplied to a harness. Keep the data directory private and protect backups accordingly.

History is retained indefinitely unless you delete a UTC date range in the Storage page. Deletion covers every service account and model in the selected range. It is permanent; there is no export feature.

For a consistent backup, stop Gyemoim and wait for it to exit, then copy the entire data directory while preserving its contents, ownership, and permissions. Include config.db and any SQLite -wal and -shm files, all of history/, and any deletion-recovery journal or other files in the data directory. Do not copy only the SQLite database or only history. Replace the target data directory with the complete backup; do not merge its files into an existing or partial data directory. Restore it under the same operating-system user and preserve its owner-only permissions, then start Gyemoim. A restored OAuth credential set may need Sign in with ChatGPT again if OpenAI has expired or revoked it. If you change the listener port, refresh the Pi setup fragment so its Base URL matches.

If shutdown or the machine stops during date-range deletion, Gyemoim completes that operation from its journal on the next startup. Queries stay unavailable while roll-forward recovery is pending; raw request recording can continue when its writer is healthy. A malformed complete history record is preserved and marks history degraded, which blocks new inference while leaving the local UI available. See [history storage and recovery](docs/history-format.md).

## Optional zstd compression

Closed history files can be compressed with the external zstd command-line program; Gyemoim does not bundle a zstd library. Install the zstd CLI from your operating system's package manager or the [upstream zstd project](https://github.com/facebook/zstd), then ensure the zstd executable is on PATH.

Without zstd, Gyemoim still starts and records new history as raw NDJSON. Queries that need existing compressed history return an explicit unavailable error. After adding zstd, restart Gyemoim so it can validate compressed files and retry pending recovery.

## Usage and history views

The Overview shows provider-reported usage and outcomes. Missing token counts remain unknown rather than zero; cached input and reasoning output are subsets of input and output. Timing shows stages Gyemoim can observe. The performance summary uses a recent sample of up to 100 requests, not archive-wide percentiles.

Request history is useful for investigating usage and repeated inputs, but Gyemoim does not automatically explain cache misses, compare requests, export history, or identify harness work outside the gateway. It records the service account and model alias, not a conversation or project identity.

## Current verification limits

The four release targets are cross-built on Linux. No native macOS runtime check is available. Live authenticated OpenAI sign-in, refresh, model catalog, inference, and a complete pi-to-Gyemoim session have not been verified with an account. See [OAuth verification notes](docs/oauth.md).

