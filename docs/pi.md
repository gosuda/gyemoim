# pi agent setup

Gyemoim generates a `models.json` fragment for pi agent 1.0.0. This first integration exports reasoning Models only. The generated provider points pi at Gyemoim's Responses API, so pi uses a Gyemoim ServiceAccount key and never receives an OpenAI OAuth credential.

## Prepare a ServiceAccount

1. Create or select a Gyemoim ServiceAccount and issue it a local API key from the Service accounts page. The plaintext key is shown once. Copy it while the issuance panel is open; Gyemoim does not retain a retrievable plaintext copy.
2. Create the Model routes you want pi to request, then grant those Models explicitly to the ServiceAccount.
3. For each reasoning Model, enter only exact metadata you can verify: positive `contextWindow` and `maxTokens`, supported `input` modalities including `text`, `reasoning: true`, and a nonempty `supportedReasoningEfforts` list. Basic Models can omit metadata; Pi setup will report missing fields for incomplete grants. Fields you leave empty fall back to a built-in catalog of verified upstream specifications — currently the GPT-6 family (`gpt-6-astra`, `gpt-6-sol`, `gpt-6-luna`, `gpt-6.1-sol`, including dated snapshot IDs) from OpenAI's published model documentation. Values you enter explicitly always win, and an explicitly invalid value is reported rather than replaced. Upstream models outside the catalog still require manual metadata.
4. Open Pi agent setup on that ServiceAccount and choose **Check Pi setup**. The response re-reads the account's current grants. It lists every granted Model, including incomplete ones and the fields each still needs. It exports only complete currently granted Models and only while the ServiceAccount is enabled.

The resulting fragment contains a `providers` object keyed by a stable identifier derived from the ServiceAccount ID. The server sends the fragment without a base URL; the UI JavaScript fills in `baseUrl` from the browser address (`window.location.origin + "/v1"`), so the saved provider points at whichever address the browser used to reach Gyemoim — loopback, LAN, or the https reverse proxy. The provider also uses `api: "openai-responses"` and the local Models' names as ids, which are the aliases pi sends back to Gyemoim. The fragment contains only Models explicitly granted to that ServiceAccount. It has no provider OAuth data, no stored local key, and no invented pricing.

## Set the local key and merge the fragment

Pi reads the configured `apiKey` from an environment variable. The UI displays a variable name such as `GYEMOIM_SERVICE_ACCOUNT_01234567_89AB_CDEF_0123_456789ABCDEF_API_KEY` and a shell command template. In the shell environment used to launch pi, replace the placeholder with the one-time Gyemoim key you copied:

```sh
export GYEMOIM_SERVICE_ACCOUNT_01234567_89AB_CDEF_0123_456789ABCDEF_API_KEY='paste-the-one-time-issued-Gyemoim-key-here'
```

Download or copy the JSON fragment. Merge its provider entry into the `providers` object in `~/.pi/agent/models.json`, preserving existing providers and settings. Gyemoim does not write that file or merge it automatically. If account grants or Model metadata change, check setup again and merge the refreshed fragment.

The generated model entries use the Pi 1.0.0 `thinkingLevelMap` field. `off` maps to `none` only when `none` is explicitly supported; the other levels map to themselves only when explicitly listed. Unsupported levels are `null`. The entries also set `compat.supportsMaxOutputTokens` and `compat.supportsLongCacheRetention` to `false`; no pricing values are generated.
