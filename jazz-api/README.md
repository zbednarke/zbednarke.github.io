# Jazz API

Private Cloud Run service for The Jazz Project. It stores campaign state and an
append-only change history in the isolated `jazz_project` database on
`parabolio-db`, and brokers private browser recordings into a dedicated GCS
bucket. Practice sessions contain session notes and structured activities;
recordings reference their parent session and carry tune, skill focus, take,
format, duration, and listening notes. A video take is stored as one logical
record with two private assets: a browser-playable video and a separate
lossless 48 kHz / 24-bit WAV master. Both assets use resumable GCS uploads and
the take becomes playable only after both have passed server-side verification.

Authenticated users can create one permanent opaque share URL per recording
asset. The public endpoint validates that bearer token and redirects to fresh
short-lived GCS access. The bucket therefore stays private while the user-facing
share URL does not expire.

The service trusts only requests carrying both headers inserted by the Caddy
gateway:

- `X-Jazz-User`
- `X-Jazz-Gateway-Key`

Runtime configuration:

- `DATABASE_URL`
- `GATEWAY_KEY`
- `GCS_BUCKET`
- `GCP_SERVICE_ACCOUNT`
- `PUBLIC_SHARE_BASE_URL` (defaults to `https://zachbednarke.com/jazz/share`)
- `FFMPEG_PATH` (defaults to `ffmpeg`; the production image includes it)
- `PORT` (defaults to `8080`)

`JAZZ_ALLOW_INSECURE_LOCAL=1` is available only for local development.

## Clip Studio renders

Clip Studio render requests are authenticated, limited to 24 clips and ten
minutes, and stream directly from the private recording bucket through FFmpeg
back into the bucket. The output is a 1080p H.264 MP4 with the separate WAV
masters encoded as lossless 48 kHz ALAC audio. Render objects live under the
`renders/` prefix and receive a GCS custom timestamp. Apply
`deploy/jazz-recordings-lifecycle.json` to the recordings bucket so those
temporary outputs are removed after one day. Cloud Run must allow long requests
for the synchronous MVP renderer; use a 60-minute request timeout.

## Playback optimization

Browser-created WebM and fragmented MP4 recordings can require long scans before
their duration and seek index are usable. `cmd/repair-video-index` losslessly
remuxes those objects, moves MP4 metadata to the front, writes WebM cues, applies
a ten-minute private browser-cache policy, and keeps each original beside the
optimized file as `video.original-unindexed.*`. It verifies that the codecs and
size remain packet-preserving and refreshes GCS metadata in Postgres. It is
dry-run by default:

```sh
go run ./cmd/repair-video-index -all
go run ./cmd/repair-video-index -id RECORDING_UUID -apply
```

`Dockerfile.repair` and `cloudbuild.repair.yaml` package the same command for
the `jazz-video-index-repair` Cloud Run job so large recordings can be repaired
inside the bucket's region. Run the job periodically with `-all -apply`; it
selects only recordings whose `video_playback_optimized` flag is false.
