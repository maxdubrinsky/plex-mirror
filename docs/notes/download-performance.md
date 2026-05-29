# Note — Download performance (glb-gdl.14)

- Date: 2026-05-29
- Epic: glb-gdl
- Scope: characterize single-stream Range-GET throughput from the remote
  (shared) Plex, identify the bottleneck, implement the low-risk wins.

## TL;DR

The engine pulls each file as a **single sequential HTTP Range GET**. That is
the right shape; the realistic ceiling is the *connection*, not our code. The
two code-level inefficiencies were (1) a small 64 KiB copy buffer and (2) an
untuned `http.Client{}` (4 KiB socket buffers, transparent gzip negotiation).
Both are now fixed (low-risk). The dominant real-world factor is **which Plex
connection we're on**: a *relay* connection is bandwidth-throttled by Plex and
will cap throughput regardless of any client tuning — discovery already prefers
direct and logs a warning when it falls back to relay.

## Baseline measurement procedure (operator, against live Plex)

We can't capture live numbers from CI/sandbox (no route to the share, token is
secret). To get a real baseline on the deployment box:

1. **Raw `curl` ceiling** — resolve a part URL and time a direct pull:
   ```bash
   # ratingKey -> Part.key via the metadata endpoint, then:
   curl -s -o /dev/null -w '%{speed_download} B/s\n' \
     -H "X-Plex-Token: $TOKEN" \
     "$PLEX_URL/library/parts/<id>/file.mkv?download=1"
   ```
2. **Engine single-stream** — queue one item and read the completion log; the
   engine now emits effective throughput per download (added in this ticket):
   ```json
   {"msg":"download: complete","throughput_mbps":, "transferred_bytes":, "elapsed":""}
   ```
3. Compare. If engine ≈ curl, we're connection-bound (expected). If engine is
   materially slower than curl, investigate client settings further.
4. **Connection type** — on boot the service logs the chosen Plex connection and
   warns `chosen Plex connection is a relay (bandwidth-throttled)` when it picked
   a relay. Relay caps (historically ~1–10 Mbps) dwarf any client-side tuning.

## Findings (code analysis)

| Factor | Before | Assessment |
|---|---|---|
| Transfer shape | single sequential Range GET, append to `.partials/*.tmp`, atomic rename | Correct. Resumable, FS-is-truth. |
| Copy buffer | `make([]byte, 64*1024)` | Small for high-BDP remote links — more loop iterations + DB-flush checks than necessary. |
| HTTP client | `&http.Client{}` (DefaultTransport) | 4 KiB socket read/write buffers; advertises gzip (pointless on already-compressed video). |
| Keep-alive | DefaultTransport pools idle conns | Fine; resumes reuse a warm conn. `ResolveDownloadURL` is re-called per attempt (cheap metadata GET), not per chunk. |
| Concurrency | `DownloadConcurrency` (default 2) across *files* | Good for queue throughput; does nothing for a single large file. |
| Connection | discovery ranks direct > relay, probes reachability | The real lever. Relay = throttled. |

### Bottleneck

For a single large file the bottleneck is the **server/connection bandwidth**,
not local CPU or syscalls — *provided* the buffers aren't pathologically small.
The 64 KiB buffer + untuned transport were the only client-side drags worth
removing. Parallel ranged segments (below) are the only client trick that can
beat a single connection, and only when Plex throttles *per-connection* rather
than per-account.

## Implemented (low-risk wins)

1. **Configurable copy buffer, default 1 MiB** (was hard-coded 64 KiB).
   `PLEXMIRROR_DOWNLOAD_BUFFER` (size suffixes K/M/G), floored at 32 KiB.
   `download.Options.BufferSize`.
2. **Tuned default transport** when no client is injected: 256 KiB socket
   read/write buffers, `DisableCompression=true`, `MaxIdleConnsPerHost=4`. No
   request `Timeout` (streaming is bounded by ctx + progress, unchanged).
3. **Throughput instrumentation**: the completion log now reports
   `transferred_bytes`, `elapsed`, and `throughput_mbps` (counting only bytes
   pulled this run, so a resume doesn't report a fake multiple). This is the
   measurement hook step 2 above relies on.

## Recommended, NOT implemented (higher risk / needs live data)

- **Parallel ranged segments per file** (split a file into N byte ranges,
  fetch concurrently, reassemble). Can beat a single connection *iff* Plex
  throttles per-connection. Adds real complexity: N partials → reassembly,
  resume bookkeeping per segment, integrity across segments, and it hammers a
  *shared* server (be a good guest). Gate on a live finding that single-stream
  is materially below the `curl` ceiling AND that a second connection actually
  goes faster. Decision deferred pending step-1/2 numbers.
- **Surface relay status in the portal** (storage/queue page badge) so a slow
  pull has an obvious cause. Cheap; fold into the UX pass (glb-gdl.15).

## Acceptance check

- Baseline procedure: documented above (operator-run; needs the live share).
- Bottleneck: identified — connection/relay-bound; client buffers were the only
  code-level drag.
- Low-risk wins: implemented (buffer, transport, instrumentation) with tests.
