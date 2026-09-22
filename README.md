# XMBridge

A small Go daemon that watches your own Mastodon, Bluesky, and X accounts and mirrors
every new post to the others, so posting on any one platform makes it appear everywhere.

```
                       ┌──────────────────────────────────────────────┐
  X (polled)  ────────▶│                                              │
  Mastodon (polled) ──▶│  engine: recognise → dedupe → prepare media  │───▶ Mastodon
  Bluesky (jetstream)─▶│          → fan out to every other platform   │───▶ Bluesky
                       └──────────────────────────────────────────────┘
```

X is **inbound only**. The bridge reads X and copies posts to Mastodon and Bluesky, but
never writes to X, because the X API's write access requires a paid tier and its video
endpoints do not expose a downloadable file. Everything else is two-way.

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
1. Create an application on your instance (Preferences → Development → New application)
   with the `read:statuses` and `write:statuses` scopes.
2. Use the generated **access token** as `mastodon.access_token`.

### Bluesky
1. Settings → App Passwords → Add App Password.
2. Use the generated password as `bluesky.app_password`. It is not your account password,
   and it can be revoked at any time. No app registration or OAuth flow is needed.

### X
Reading is enough, so app-only bearer auth works:

1. Create an app in the X developer portal.
2. Copy the **bearer token** (app-only) into `twitter.bearer_token`.
3. Find your numeric user ID and set `twitter.user_id`.

OAuth 1.0a user-context credentials (`consumer_key`, `consumer_secret`, `access_token`,
`access_token_secret`) are also supported and are used instead of the bearer token when
all four are present.

Reading your own timeline is subject to X's access tier. The Free tier is heavily rate
limited (of the order of a couple of dozen read requests per 15 minutes), so the bridge
parks itself until the window resets rather than hammering the endpoint. Expect X posts to
appear a little later than the others on a low tier.

## Running it

### Docker Compose

```bash
cp config.example.yaml config.yaml
export XMBBRIDGE_MASTODON_ACCESS_TOKEN=...
docker compose up -d --build
docker compose logs -f
```

State lives on the `bridge-data` volume; the config is mounted read-only.

### systemd

```bash
sudo useradd --system --home /var/lib/xmbbridge xmbbridge
sudo install -d -o xmbbridge -g xmbbridge /var/lib/xmbbridge /etc/xmbbridge
sudo install -m 600 -o xmbbridge config.yaml /etc/xmbbridge/config.yaml
sudo install -m 600 -o xmbbridge /dev/null /etc/xmbbridge/xmbbridge.env   # secrets go here
sudo cp deploy/xmbbridge.service /etc/systemd/system/
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
  have a real MP4 URL and are transcoded normally.
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
re-bridged (the loop guard), identical content is deduped once, routing excludes X and the
origin, replies thread onto bridged parents, failed bridges stay retryable, images are
compressed to fit a byte budget, and video transcoding produces a real MP4.
