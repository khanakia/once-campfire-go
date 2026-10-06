# Go vs Rust under gzip — 2026-10-06

The published cross-language comparison (once-campfire-elixir `bench/results/ruby-elixir-go-rust-20261004`) measured Go at 3,860 room pages/s against Rust's 36,260. That harness sends `Accept-Encoding: gzip` by default; this port's own `bench/application` sent `identity`, so the gzip path had never been optimized. This pass finds where the Go port was slower than the Rust port and fixes it.

## Results

macOS (Apple M1 Max, 10 cores), no CPU pinning, `GOMAXPROCS=4` / `TOKIO_WORKER_THREADS=4`, the Rust parity seed, the Rust loadgen against the public front port, 16 clients, three interleaved repetitions of five seconds. Medians. CPU is the server process's CPU time per successful request, comparable whatever cores an app uses. Raw samples: [final.json](final.json), [final.log](final.log).

| Route | gzip | Go before req/s | Go before CPU µs | Go after req/s | Go after CPU µs | Rust req/s | Rust CPU µs |
|---|---|---:|---:|---:|---:|---:|---:|
| room_show | 1 | 2,803 | 1,384 | 14,681 | 244 | 10,817 | 249 |
| messages_page | 1 | 3,720 | 1,032 | 17,299 | 207 | 13,439 | 229 |
| search | 1 | 4,263 | 852 | 12,502 | 290 | 8,033 | 443 |
| sidebar | 1 | 7,936 | 423 | 15,582 | 219 | 10,049 | 252 |
| room_show | 0 | 8,413 | 412 | 10,088 | 313 | 8,831 | 319 |
| messages_page | 0 | 11,210 | 305 | 14,373 | 255 | 11,542 | 285 |
| search | 0 | 6,395 | 584 | 11,627 | 309 | 8,237 | 457 |
| sidebar | 0 | 8,008 | 398 | 16,333 | 220 | 9,965 | 260 |

Go before is upstream `8d2f7f2`; Rust is `once-campfire-rust` `ccece30` (after PR #43). The workstation carried other load (load average around 15), so absolute rates move between runs; the before/after and Go/Rust ordering held across every run. These are not the Linux numbers of the published table, which pin four CPUs.

## What was slow, and the fix

1. **Gzip of every page from scratch.** Rack::Deflater's Go equivalent ran `compress/gzip` level 6 over each ~400 KB page, ~1 ms of CPU per request. `internal/gzsplice` now deflates each page part once, with the previous part as the preset dictionary and a sync flush, and keeps the piece keyed by the SHA-256 of the part and its predecessor; a response joins stored pieces and combines their CRC-32s (zlib's `crc32_combine` with stored shift operators). A cached room page costs ~264 ns to gzip. Whole bodies with a body-digest ETag (the sidebar) reuse the same cache. Other gzip goes through `klauspost/compress`.
2. **A per-request timestamp in the room page.** `data-refresh-room-loaded-at-value` was `Now()`; Rails (`messages_helper.rb`) and the Rust port use `room.updated_at`. Every room page was unique, so neither the room-shell cache nor gzip reuse could work. It now matches Rails and Rust.
3. **Spliced gzip refused `Last-Modified` responses.** messages#index sets it (`fresh_when`) and Rack::Deflater puts it in the gzip header mtime; the member header is now written per response.
4. **A goroutine per query.** `database/sql` starts a watcher goroutine for any query whose context can be cancelled, and every read used the request context. Reads now run with `context.WithoutCancel`; like the Rust port's queued reads, a read is not interrupted when its client disconnects.
5. **SQLite's memory statistics.** With `SQLITE_DEFAULT_MEMSTATUS` on (the default), every SQLite allocation takes one process-wide mutex; under concurrent reads it was the main contention. The build entry points now set `CGO_CFLAGS="-O2 -g -DSQLITE_DEFAULT_MEMSTATUS=0"`, guarded by `internal/database/build_flags_test.go`.
6. **Extra queries.** Authentication read the session twice (user, then `last_active_at`); the sidebar queried members once per direct room; search loaded every hit's rich-text body and author although rendered messages come from the fragment cache. Now one session query, one member query for all direct rooms (same order as before), and `SearchReferences` (ids only, misses hydrated as on the room page).
7. **Room shell work per request.** The cached shell is stored already split around the message list with its SHA-256s, removing a 34 KB `strings.ReplaceAll`, a marker search and two hashes per request. The splice writes each gzip member with one `Write`.

Tried and dropped: eight read connections instead of `GOMAXPROCS` (more CPU per request, no throughput gain) and `_mutex=no` (mixed).

## Verification

- `bin/check` (gofmt, vet, race tests) and the websocket tests pass. The one failure on this machine, `internal/storage` `TestMediaMetadataAndTrackedPreview`, is a broken local ffmpeg install (`libjxl.0.11.dylib` missing) and fails identically on the unchanged upstream tree.
- New tests: gzsplice decoding, cache reuse, size against whole-body gzip, generations, mtime, and CRC combination; gzip responses through Deflate equal to identity (recorded, whole, empty, Last-Modified), break-verified; session activity and refresh; batched direct-room members equal per-room queries including order; `SearchReferences` equal to `Search`; reads ignoring cancellation, break-verified; the build flag guard, break-verified.
- On the parity seed, room, messages, sidebar and search responses from the new binary equal upstream's byte for byte once the random port and the corrected `loaded-at` value are normalized; gzip bodies decode to the identity bytes; search returns the same messages as Rust.

## Reproducing

`bench/macos/macbench.py ROOT APP=BINARY...` and `bench/macos/fetchall.py ROOT OUTDIR APP=BINARY...` take a `ROOT` directory containing a `rust/` checkout of once-campfire-rust with `parity/.seed/default` built (`parity/bin/seed build default`), the release binary (`cargo build --release -p campfire`) and the loadgen (`cd bench/loadgen && cargo build --release --target-dir ../../target/bench`).

```sh
bench/macos/macbench.py ROOT go-before=/path/to/upstream/campfire go-after=./campfire \
  rust=ROOT/rust/target/release/campfire --gzip 1 0 --conc 16 --reps 3 --secs 5 --out results.json
```
