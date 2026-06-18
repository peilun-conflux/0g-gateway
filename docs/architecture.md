# Architecture

A developer-facing overview of the gateway's design and code structure. For deploying,
configuring, and using it, see the [README](../README.md); for the maintainer's manual
(invariants, pitfalls, conventions) see [`../CLAUDE.md`](../CLAUDE.md); for the original
design rationale see `../0g-gateway-design.md`.

## What it is

**An S3-compatible endpoint in front of 0G decentralized storage.** Externally clients use
S3 `bucket + key` (e.g. `bucket` + `user/123/a.png`); internally the gateway is
**content-addressed**: an object's true key is the merkle root of its bytes (`0x…`, generated
server-side, giving free deduplication and verifiability). The gateway maintains the
bucket/key → root mapping transparently. Pure Go, a single process, with an embedded bbolt
store for metadata and no external database.

> An earlier revision also exposed a native HTTP API (`/objects` + `/kv` + HMAC-signed URLs).
> Because "S3 is the only entry point," that surface was removed entirely; the sole external
> interface today is S3 (`internal/s3gw`).

## Overview

```
                 ┌────────────────────── gateway process ──────────────────────┐
   S3 / OBS SDK  │                                                              │
 ──PUT/GET/DEL─► │  s3gw (gofakes3) ──► object service ──┬──► bbolt (metadata/queues/index) │
   (bucket/key)  │  (S3 protocol + images) (ingest/read)  └──► local cache (disk files)     │
                 │                                                              │
                 │  uploader worker ──(Chain interface)──► chain backend ───────┼──► 0G network
                 │  (background goroutine: batch/reconcile/retry)               │   (on-chain tx + nodes)
                 └──────────────────────────────────────────────────────────────┘
```

- **s3gw** — the S3 protocol layer (gofakes3 decodes XML/Range/aws-chunked/multipart), mapping
  bucket+key operations onto the object service. Four middlewares run in front of gofakes3:
  cache backpressure (sheds uploads with `503 SlowDown` when the cache is full of un-evictable
  data), `X-Amz-Copy-Source` normalization, Huawei-style image processing
  (`?x-image-process=image/resize,...`), and XML-response Content-Type normalization.
- **object service** — the ingest pipeline (hash / dedup / cache) and reads (cache hit, or cold
  restore from 0G).
- **store (bbolt)** — object metadata, the two task queues, the bucket registry, and the
  bucket/key → root index. **Every state change is one transaction.**
- **uploader worker** — a single background goroutine that batches queued objects, submits them
  to 0G, reconciles, and retries.
- **chain backend** — wraps the 0G SDK and performs the actual upload, status queries, and
  proof-verified downloads.

## Data flow

**Upload (readable immediately; the upload to 0G happens in the background)**

```
PUT /bucket/foo
  ├─ stream to a temp file while computing SHA256 + MD5 (the S3 ETag)
  ├─ dedup by SHA: already-seen content is reused, never re-uploaded
  ├─ compute the merkle root → atomic rename into the local cache
  ├─ write metadata (pending) + enqueue for upload + record bucket/key → root
  └─► 200                                       ← downloadable now (from the local cache)

     …later, in the background worker…
  batch → one on-chain tx submits the whole batch → onchain → poll finality → finalized
```

**Download (local-first, restore on miss)**

```
GET /bucket/foo ─► look up bucket/key → root ─► local cache hit?
                                                 ├─ yes: serve after a size check (Range / HEAD)
                                                 └─ no:  download from 0G (with merkle proof)
                                                         → write back to cache → serve
```

## Object lifecycle

```
pending ──► submitted ──► onchain ──► finalized   (terminal: success)
(cached,    (handed to    (tx mined,   (storage nodes confirmed)
 awaiting    the chain     awaiting
 upload)     backend)      finality)
   └──────────────────────────────► failed         (terminal: retries exhausted / pruned)
```

- **Crash recovery** — the task queues live in bbolt. After a restart the worker first
  reconciles `submitted`/failed objects (those already on-chain are completed by re-uploading
  only their fragments via `SkipTx`; those not on-chain get their tx re-sent) before deciding
  whether to re-upload — it never blindly re-submits. If a node is unreachable during
  reconciliation, it **skips this round and leaves the item queued** rather than sending a tx
  in an unknown state.
- **Deletion** — removes the bucket/key mapping only; 0G content is content-addressed and
  cannot be physically erased.
- **Bounded cache** — the on-disk cache is capped (`cache_max_bytes`). When it grows past the
  cap, the least-recently-used *finalized* objects' cache files are evicted (they restore from
  0G on the next read); non-finalized objects are never evicted (their cache file is the only
  copy). If the cache fills with not-yet-finalized objects, new uploads get a retryable
  `503 SlowDown` until the upload worker drains the backlog.
- **`pruned`** (an object dropped by a storage node) is an alarm condition; archival
  deployments must plan capacity accordingly.

## Code structure

```
cmd/gateway        process entry point, config loading, graceful shutdown
internal/s3gw      S3 protocol layer (gofakes3 backend) + middlewares (copy-source / image / XML content-type)
internal/object    ingest/read pipeline (dedup, cache, cold restore)
internal/store     bbolt metadata, upload/finalize queues, bucket + key→root index
internal/uploader  background batch-upload worker (batch / reconcile / retry / finality polling)
internal/chain     0G SDK wrapper (upload / status query / proof-verified download)
internal/imageproc image resizing (pure standard-library image/resize)
integration        live e2e + Huawei OBS SDK (Java/Node.js) compatibility tests
```

Dependency direction: `s3gw → object → store`; `uploader → store` (plus the `Chain`
interface); `chain` implements `uploader.Chain` and is injected by `main`. The `uploader`
package does **not** depend on the `chain` package — they are decoupled through an interface,
which keeps the worker easy to test with a fake backend.

## Design notes

- **The SDK version is pinned to `v1.4.3-testnet`.** `@latest` resolves to v1.3.0, whose API is
  incompatible; check the design document before upgrading.
- **Avoid the SDK's `Must*` constructors** — they call `os.Exit` on error and bypass error
  returns; always use the error-returning variants.
- **Default per-object cap is 4 GiB** (the SDK fragment size), guaranteeing one object → one
  root. Larger files would need a manifest design and are deliberately out of scope. There is
  no multipart backend (gofakes3 buffers fragments in memory — small files only).
- **Encryption (not implemented, seam reserved)** — gateway-side encryption would touch only
  two places: a transform before `Put` writes the cache (cache stores ciphertext, the root is
  computed over ciphertext) and a decrypt before `Open` streams back; the rule is "wrap once,
  ciphertext is canonical." Metadata is stored as JSON in bbolt, so new fields stay
  backward-compatible.
- **Confidentiality** — in production the node RPCs are confined to an internal network
  (reachable only by the gateway), matching OBS private-bucket semantics. The S3 endpoint does
  not verify signatures and must be bound to an internal interface only.
