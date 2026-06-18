# 0G Storage Gateway

An S3-compatible object storage gateway backed by [0G](https://0g.ai) decentralized storage.
Point any S3 client — including the Huawei OBS SDK — at it and use the familiar `bucket + key`
API; data is persisted to a 0G deployment (self-hosted storage nodes plus a Flow contract on an
EVM chain) and callers never have to learn 0G. It's a single Go binary with an embedded
[bbolt](https://github.com/etcd-io/bbolt) metadata store and no external database.

> [!WARNING]
> The endpoint does **not verify request signatures** (demo posture) — any non-empty access key
> is accepted. Bind it to an **internal interface only** (e.g. `127.0.0.1:8080`), never `0.0.0.0`
> or a public address.

## Quick start

Requires **Go 1.24+** and a reachable 0G deployment: one or more storage-node RPC endpoints, an
EVM host-chain RPC, and a signer private key funded with gas on that chain.

```bash
make build                              # -> bin/gateway
cp config.example.yaml config.yaml      # then fill in nodes / eth_rpc / private_key
./bin/gateway --config config.yaml      # serves the S3 endpoint on :8080
```

`./config.yaml` is auto-detected if `--config` is omitted, and every setting can come from
environment variables instead (see below).

## Configuration

Settings resolve in increasing precedence: **defaults → config file → environment variables**,
so an env var always overrides the file (handy for injecting `ZGS_PRIVATE_KEY` into a container
while the rest stays in a mounted `config.yaml`). The full, commented reference lives in
[`config.example.yaml`](config.example.yaml) and [`.env.example`](.env.example).

**Required**

| Config key    | Environment variable | Description |
|---------------|----------------------|-------------|
| `nodes`       | `ZGS_NODES`          | 0G storage-node JSON-RPC endpoints (env: comma-separated) |
| `eth_rpc`     | `ZGS_ETH_RPC`        | Host-chain (EVM) RPC where the Flow contract lives |
| `private_key` | `ZGS_PRIVATE_KEY`    | Signer key (hex, no `0x` prefix); must hold gas on the host chain |

**Common optional** (defaults in parentheses): `listen` (`:8080` — ⚠️ bind internal-only),
`data_dir` (`./data`), `max_size` (`4 GiB` per object), `cache_max_bytes` (`10 GiB` local cache
cap; `0` = unbounded). The example files document the rest (batch size, flush interval, retries,
replica count).

## Usage

Objects are addressed as standard S3 `bucket + key` with **path-style** addressing — any S3
client works:

```bash
aws --endpoint-url http://localhost:8080 s3 cp ./hello.txt s3://demo/hello.txt
aws --endpoint-url http://localhost:8080 s3 cp s3://demo/hello.txt ./out.txt
```

**Huawei OBS SDK:** set `signature=v2` + `path_style=true` and point the endpoint at the gateway
(the access key can be any non-empty value). Connection snippets for Java/Node.js, the full
operation support matrix, image resizing, and caveats are in
[docs/migration-from-obs.md](docs/migration-from-obs.md).

Good to know:

- **Writes are asynchronous** — a `PUT` returns and is readable as soon as it's cached locally;
  the upload to 0G completes in the background, so an object isn't necessarily finalized
  on-chain the instant `PUT` returns.
- **Deduplicated and re-runnable** — identical content uploads to 0G once, so an interrupted
  bulk migration can be re-run without re-uploading or re-spending gas.
- **Bounded local disk** — the cache is capped; finalized objects are evicted and restored from
  0G on demand. During heavy write bursts the gateway may return a retryable `503 SlowDown`,
  which S3 SDKs back off and retry automatically.
- **Not supported** — empty objects, multipart upload, versioning, and `ListObjects` pagination.

## Testing

```bash
make test     # unit + integration (fake 0G backend; includes real OBS SDK runs if a JDK/Node is present)
make lint     # gofmt + go vet
make e2e      # live end-to-end against real 0G (ZGS_E2E=1; needs a funded ZGS_PRIVATE_KEY)
```

## Documentation

- [Architecture](docs/architecture.md) — how it works, design, and code layout
- [Migrating from Huawei OBS](docs/migration-from-obs.md) — OBS SDK setup, support matrix, caveats
- [S3 API reference](docs/s3-api.md) — capabilities and differences from OBS at a glance
- [`CLAUDE.md`](CLAUDE.md) — maintainer's manual; `0g-gateway-design.md` — original design notes
