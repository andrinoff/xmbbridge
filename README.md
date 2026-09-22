# XMBridge

A small Go daemon that watches your own Mastodon, Bluesky, and X accounts and mirrors
every new post to the others, so posting on any one platform makes it appear everywhere.

```
                       ┌──────────────────────────────────────────────┐
  X (polled)  ────────▶│                                              │───▶ Mastodon
  Mastodon (polled) ──▶│  engine: recognise → dedupe → prepare media   │───▶ Bluesky
  Bluesky (jetstream)─▶│          → fan out to every other platform    │───▶ X (OAuth 1.0a)
                       └──────────────────────────────────────────────┘
```

X is read-only unless you give it the full OAuth 1.0a user-context credential set. With
the bearer token alone the bridge reads X and copies posts to Mastodon and Bluesky; with
the OAuth 1.0a keys configured, X also becomes a destination.

## What it does

- **Reads** new posts from each enabled platform:
  - Mastodon: polls your account's statuses API (`since_id` cursor).
  - Bluesky: tails the public Jetstream relay over a websocket, filtered to your DID
    (real-time, no polling, no credentials).
  - X: polls your user timeline (`since_id` cursor), honouring rate-limit headers.
- **Writes** each post to every other enabled platform, truncating text to fit and
  re-encoding media to fit each destination's limits.
- **Never echoes its own output.** Every post the bridge creates is recorded against the
  post it came from; when a listener later sees that post it recognises it as its own
  work and stops. Without this a two-way bridge would copy the same post forever.
- **Avoids accidental duplicates.** Identical text bridged within `dedup_window` is
  copied once, which covers posting the same thing by hand on two platforms at once.
- **Threads replies** onto the bridged copy of their parent when the parent was bridged
  too, and posts them standalone otherwise.
- **Keeps state** in SQLite so restarts resume from where they left off.

## Quick start

```bash
git clone https://github.com/andrinoff/xmbbridge && cd xmbbridge

cp config.example.yaml config.yaml
$EDITOR config.yaml          # enable platforms, add handles; keep secrets in the env

make build
make check                   # validates the config and authenticates with each platform
make run                     # start bridging
```

Secrets are read from the environment and always override the file, so `config.yaml` can
stay world-readable:

```bash
export XMBBRIDGE_MASTODON_ACCESS_TOKEN=...
export XMBBRIDGE_BLUESKY_APP_PASSWORD=...
export XMBBRIDGE_TWITTER_BEARER_TOKEN=...
export XMBBRIDGE_TWITTER_USER_ID=...
```

On first run the bridge starts from *now*: it logs the newest post it sees and does not
replay history. Set `backfill: true` to copy existing posts instead (Mastodon and X only;
Bluesky's listener is a live tail and has nothing to replay).

## Credentials

### Mastodon
1. Create an application on your instance (Preferences → Development → New application).
   Grant the `read:statuses`, `write:statuses`, and `write:media` scopes. `write:media` is
   required for image and video uploads.
2. Use the generated **access token** as `mastodon.access_token`.

Without the web UI, the same token can be obtained from the API. You will be shown a URL
to approve in a browser, then paste the resulting code back:

```bash
INSTANCE=https://mastodon.social
curl -s -X POST "$INSTANCE/api/v1/apps" \
  --data-urlencode "client_name=xmbbridge" \
  --data-urlencode "redirect_uris=urn:ietf:wg:oauth:2.0:oob" \
  --data-urlencode "scopes=read:statuses write:statuses write:media"
# prints client_id and client_secret; put them in the URL below and approve it
echo "$INSTANCE/oauth/authorize?client_id=CLIENT_ID&scope=read:statuses+write:statuses+write:media&redirect_uri=urn:ietf:wg:oauth:2.0:oob&response_type=code"
# after approving, exchange the code for a token
curl -s -X POST "$INSTANCE/oauth/token" \
  --data-urlencode "client_id=CLIENT_ID" \
  --data-urlencode "client_secret=CLIENT_SECRET" \
  --data-urlencode "grant_type=authorization_code" \
  --data-urlencode "code=THE_CODE" \
  --data-urlencode "redirect_uri=urn:ietf:wg:oauth:2.0:oob"
```

### Bluesky
1. Go to <https://bsky.app/settings/app-passwords> → **Add App Password**, name it
   `xmbbridge`, and copy the generated password.
2. Use your handle as `bluesky.handle` and the generated password as
   `bluesky.app_password`. It is not your account password, and it can be revoked at any
   time. No app registration or OAuth flow is needed.

### X
Reading works with an app-only bearer token; **posting and media uploads require the full
OAuth 1.0a user-context set**, because the write endpoints reject app-only auth.

1. Sign up at <https://developer.x.com>, create a Project and an App. Set the App's user
   authentication settings to **Read and write** before generating tokens.
2. From **Keys and tokens**, copy:
   - the **Bearer Token** (only needed if you want read-only X, or as a fallback)
   - the **API Key** and **API Key Secret** → `consumer_key`, `consumer_secret`
   - the **Access Token** and **Access Token Secret** → `access_token`,
     `access_token_secret`
3. Look up your numeric user ID and set `twitter.user_id`:

```bash
curl -s "https://api.twitter.com/2/users/by/username/YOURHANDLE" \
  -H "Authorization: Bearer $XMBBRIDGE_TWITTER_BEARER_TOKEN"
# => {"data":{"id":"1234567890","name":"...","username":"YOURHANDLE"}}
```

Gotcha: an access token inherits the app's permission level **at the moment it is
generated**. If the app was created as read-only, change it to Read and write and
regenerate the Access Token and Secret, or every write returns 403.

Both reading and posting consume your X API credits, and a poll that finds nothing still
costs a request. If you pay per request, raise `twitter_poll_interval` (for example to
`30m`) to stretch the budget; X posts will simply arrive later than the others. When
credits run out the listener logs `API credits are exhausted; pausing the listener` and
rests for an hour at a time until you top up — the Mastodon and Bluesky halves carry on
regardless.

## Running it

### Docker Compose

```bash
cp config.example.yaml config.yaml
export XMBBRIDGE_MASTODON_ACCESS_TOKEN=...
docker compose up -d --build
docker compose logs -f
```

State lives on the `bridge-data` volume; the config is mounted read-only.

### Ubuntu server, one command

Copy the repository to the server and run the installer as root. It installs ffmpeg,
builds the binary, creates the service user, installs the unit, and starts the service:

```bash
# from your workstation
tar --exclude ./data --exclude ./bridge --exclude ./config.yaml -czf xmbbridge.tgz .
scp xmbbridge.tgz you@server:~/ && scp config.yaml you@server:~/xmbbridge/

# on the server
mkdir -p ~/xmbbridge && tar xzf ~/xmbbridge.tgz -C ~/xmbbridge && cd ~/xmbbridge
sudo ./deploy/install-ubuntu.sh config.yaml
sudo systemctl restart xmbbridge          # after filling in /etc/xmbbridge/xmbbridge.env
journalctl -u xmbbridge -f
```

The script is safe to re-run: it keeps an existing config, env file, and database. It also
prints a credential-check command that runs `-check` as the service user.

### systemd, step by step

If you would rather do it by hand:

```bash
sudo apt install -y ffmpeg
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /usr/local/bin/bridge ./cmd/bridge

sudo useradd --system --home-dir /var/lib/xmbbridge --create-home --shell /usr/sbin/nologin xmbbridge
sudo install -d -o xmbbridge -g xmbbridge /var/lib/xmbbridge
sudo install -d /etc/xmbbridge
sudo install -m 600 -o xmbbridge -g xmbbridge config.yaml /etc/xmbbridge/config.yaml
sudo install -m 600 -o xmbbridge -g xmbbridge /dev/null /etc/xmbbridge/xmbbridge.env

sudo cp deploy/xmbbridge.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now xmbbridge
```

The bridge only needs outbound network access and a writable state directory.

### Command line

```
bridge [-config path] [-check] [-log-level level]
```

| Flag | Meaning |
| --- | --- |
| `-config` | Path to the YAML config. Defaults to `config.yaml`. Pass `-config ""` to run entirely from environment variables. |
| `-check` | Load the config, authenticate with every enabled platform, print the routing table, and exit. |
| `-log-level` | Override `log_level` (`debug`, `info`, `warn`, `error`). |

## Configuration

See [`config.example.yaml`](config.example.yaml) for the annotated version. The essentials:

| Key | Default | Meaning |
| --- | --- | --- |
| `storage` | `data/bridge.db` | SQLite file holding cursors, the bridged map, and dedup entries. |
| `poll_interval` | `30s` | Mastodon poll period. |
| `twitter_poll_interval` | `90s` | X poll period; rate-limit headers can push this out. |
| `dedup_window` | `5m` | How long identical text counts as a duplicate. |
| `backfill` | `false` | Bridge posts that already existed at first launch. |
| `media.image_max_dimension` | `2000` | Long edge for re-encoded images. |
| `media.ffmpeg_path` | `ffmpeg` | Binary used for video; video is skipped when absent. |
| `routes` | derived | Optional override of which platforms mirror which. |

Media budgets are per platform (`max_image_bytes`, `max_video_bytes`,
`max_video_duration`, `video`), so a Mastodon instance with small limits or a Bluesky
account with video disabled can be configured without touching code.

## Behaviour and limits

- **Deletes and edits are not propagated.** A post removed on the origin stays on the
  other platforms. The bridged map is kept so the bridge can recognise its own posts.
- **Truncation is per destination.** Mastodon allows 500 characters and Bluesky 300, so a
  long Mastodon post is shortened with an ellipsis before it reaches Bluesky. Cuts never
  land inside a URL; a URL that would be split is dropped whole.
- **Media is re-encoded, not linked.** Images are downscaled and re-compressed until they
  fit the destination's byte budget (Bluesky caps image blobs at 2MB); video is
  transcoded to H.264/AAC MP4 with duration and resolution capped. An attachment that
  cannot be made to fit is dropped with a warning and the text still goes out.
- **X video becomes its poster frame.** The v2 API exposes no downloadable video file for
  tweets, only a preview image, so X videos are copied as still images. Animated GIFs do
  have a real MP4 URL and are transcoded normally. Video **into** X uses the chunked
  upload endpoint with a 140 second cap and alt text is attached where available.
- **Truncation to 280 characters** on X is rune-based and conservative: X counts URLs as
  23 characters regardless of length, so a post that barely fits elsewhere may be cut
  slightly shorter than X itself would require.
- **Bluesky is live-only.** The Jetstream listener has no historical replay, so `backfill`
  does not apply to it.
- **Replies are opt-in** per platform via `include_replies`.
- **A failed copy is retried and then recorded.** Retries use exponential backoff, and a
  permanently failed pair is left in the `bridged` table with `status = 'error'` so it can
  be inspected:

  ```bash
  sqlite3 data/bridge.db "select origin_platform, origin_id, target_platform, error
                          from bridged where status = 'error'"
  ```

## Layout

```
cmd/bridge              entrypoint: flags, config, wiring, signals
internal/config         typed config, environment overrides, validation
internal/model          the platform-neutral Post / Media / Outbound types
internal/store          SQLite: cursors, bridged map, dedup, parent lookups
internal/feed           the engine: fan-out, loop prevention, media preparation
internal/media          download, image fitting, video transcoding via ffmpeg
internal/text           HTML unstyling, rune-safe truncation, content hashing
internal/platform       shared polling, backoff, id comparison, cursor state
internal/platform/...   the Mastodon, Bluesky, and X adapters
```

## Development

```bash
make test        # unit tests
make test-race   # the same under the race detector
make vet
make fmt
```

The test suite covers the properties that matter for correctness: a bridged post is never
re-bridged (the loop guard), identical content is deduped once, routing excludes the origin
and any platform without write credentials, replies thread onto bridged parents, failed
bridges stay retryable, images are compressed to fit a byte budget, video transcoding
produces a real MP4, and the X posting path (simple and chunked uploads, processing waits,
reply threading, credit exhaustion) is exercised against a fake of both X endpoints.
