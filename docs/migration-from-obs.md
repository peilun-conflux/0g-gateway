# Migrating from Huawei OBS

This guide is for **backend services migrating off Huawei OBS**. It explains how to connect to
the gateway with the **Huawei OBS SDK**, which operations are supported, and the caveats you
must know before integrating.

The gateway exposes an **S3-compatible protocol endpoint**. Huawei OBS is highly
S3-compatible, so the OBS SDK works directly once you point its endpoint at the gateway and
switch to **S3 V2 signing + path-style** addressing — you keep your existing `bucket + key`
programming model. Internally the gateway maps bucket+key to 0G decentralized storage
(content-addressed, automatically deduplicated), transparently to you.

> ⚠️ **Signatures are not verified** (demo posture, see [§5](#5-caveats-read-before-integrating)).
> Any access key is accepted, so write security depends entirely on **binding to an internal
> interface only**. Bind the gateway to an internal address (e.g. `127.0.0.1:8080`) — never
> `0.0.0.0` or a public address.

---

## 0. Support at a glance

| | Status |
|---|---|
| Verified SDKs | Huawei OBS **Java** SDK `esdk-obs-java-bundle` (tested with **3.21.11 and 3.25.5**; the integration partner uses Java 8 + ≥3.21.11) and the **Node.js** SDK `esdk-obs-nodejs 3.26`, both with real-SDK tests under `integration/` |
| Verified coverage | create-bucket / put / get / Range / HEAD / list / copy / presigned GET / delete — **all pass** on both SDKs |
| Required config | path-style + S3 V2 signing (see [§1](#1-sdk-connection-configuration)) |
| Not supported | multipart upload, empty objects, list pagination, versioning |
| Security | **no signature verification** (demo) — bind internal-only; presigned URLs return objects but are **not enforced** (not an access-control mechanism) |

---

## 1. SDK connection configuration

The Huawei OBS SDK defaults to OBS's proprietary signing protocol; you must switch it to
**S3 V2 signing + path-style**.

### 1.1 Huawei OBS Java SDK (used by the partner; verified on real hardware)

`esdk-obs-java-bundle` (tested with 3.21.11 and 3.25.5; partner uses Java 8 + ≥3.21.11):

```java
import com.obs.services.ObsClient;
import com.obs.services.ObsConfiguration;
import com.obs.services.model.AuthTypeEnum;

ObsConfiguration config = new ObsConfiguration();
config.setEndPoint("<gateway-host>");      // hostname/IP only, no scheme
config.setEndpointHttpPort(8080);          // port
config.setHttpsOnly(false);                // plain HTTP on an internal network
config.setPathStyle(true);                 // required: path-style
config.setAuthTypeNegotiation(false);      // don't negotiate; pin the signing type
config.setAuthType(AuthTypeEnum.V2);       // required: S3 V2 signing (not OBS's proprietary protocol)
ObsClient client = new ObsClient("anyAK", "anySK", config); // AK/SK: any non-empty value
```

Key points:

- **`setPathStyle(true)`** + **`setAuthType(AuthTypeEnum.V2)`** + **`setAuthTypeNegotiation(false)`**
  are all mandatory.
- You do **not** need `setVerifyResponseContentType(false)`: the gateway already normalizes the
  `Content-Type` of its XML responses to `application/xml` server-side, so the SDK works with
  its **default strict verification**. (Without that fix, the SDK would reject gofakes3's
  sniffed `text/xml; charset=utf-8` with "Expected XML document response".)
- The access key can be any non-empty value — the gateway does not verify signatures (see
  [§5.2](#52-no-signature-verification-write-security-via-network-isolation)).

> The verified harness is `integration/testdata/obs-java/ObsCompatTest.java` (driven by
> `integration/obssdkjava_test.go`) and covers every operation in [§2](#2-supported-operations-verified-via-the-huawei-obs-sdk).

### 1.2 Huawei OBS Node.js SDK (verified on real hardware)

```js
const ObsClient = require('esdk-obs-nodejs');

const client = new ObsClient({
  access_key_id:     'anyAK',                       // any non-empty value; signatures not verified
  secret_access_key: 'anySK',
  server:            'http://<gateway-host>:8080',  // full URL (scheme + port)
  signature:         'v2',                          // required; the default 'obs' does not work
  path_style:        true,                          // required; virtual-host style is unsupported
});
```

When the host is an **IP**, the SDK automatically selects path-style and disables signing
negotiation; with a domain name, set the two options above explicitly.

### 1.3 Other S3 SDKs (AWS SDK, boto3, …)

The principle is the same: point the endpoint at the gateway, force path-style, and use any
non-empty access key. These are **not individually verified** — validate connectivity with the
`aws s3api` commands in [§6](#6-verify-connectivity) before integrating.

---

## 2. Supported operations (verified via the Huawei OBS SDK)

The following calls pass against the real Huawei OBS **Java** and **Node.js** SDKs
(`integration/testdata/obs-java/ObsCompatTest.java` and `…/obs-js/obs_sdk_test.js`):

| OBS SDK method | Behavior / notes |
|---|---|
| `createBucket` | Create a bucket. Returns `BucketAlreadyExists` if it exists. You can also skip this — see auto-create in [§3](#3-implemented-but-not-individually-driven-via-the-obs-sdk). |
| `putObject` | Upload an object. `ContentType` etc. are stored as metadata and returned on download. **Rejects 0-byte objects** (see [§5.4](#54-empty-0-byte-objects-are-rejected)). |
| `getObject` | Download an object; **supports `Range`** (resumable / chunked). |
| `getObjectMetadata` | Fetch object headers (HEAD): size, Content-Type, ETag — does not trigger a restore from 0G. |
| `listObjects` | List objects; **supports `Prefix` filtering**; **no pagination** (see [§5.5](#55-no-list-pagination)). |
| `deleteObject` | Delete an object (a subsequent GET on that key returns 404). |
| `copyObject` | Server-side copy; **zero-copy** (content-addressed, identical bytes are not re-uploaded). Works through the OBS SDK after the gateway normalizes `X-Amz-Copy-Source` (see [§5.1](#51-copyobject-is-supported-an-earlier-limitation-is-fixed)). |
| `createSignedUrlSync` (Method=GET) | Generate a presigned download URL; returns the object correctly. ⚠️ But it is **not enforced** — see [§5.3](#53-presigned-get-works-but-is-not-access-control). |

> **ETag = the object's content MD5** (computed by the gateway during ingest on `PUT`).
> **Asynchronous semantics:** `putObject` returning success means the object is cached locally
> and **immediately readable**; the actual write to 0G completes asynchronously in the
> background (see [§5.6](#56-uploads-are-asynchronous-put-return--persisted-on-chain)).

---

## 3. Implemented, but not individually driven via the OBS SDK

These operations are implemented in the backend and covered by Go unit tests, but are not each
driven by the OBS SDK harnesses above. They should work; verify them yourself before relying
on them.

| Capability | Typical OBS SDK call | Notes |
|---|---|---|
| List buckets | `listBuckets` | List all buckets |
| Probe bucket | `headBucket` | Whether a bucket exists |
| Delete bucket | `deleteBucket` | **Empties only**; non-empty returns `BucketNotEmpty`, missing returns `NoSuchBucket` |
| Bulk delete | `deleteObjects` | Deleting a missing key is not an error (S3 semantics) |
| Conditional write | `putObject` + `If-Match` / `If-None-Match` | Supported, but **not atomic** with the write (see [§5.7](#57-conditional-writes-are-not-atomic)) |
| Auto-create bucket | — | A `PUT` to a **non-existent bucket auto-creates it**, accommodating apps that assume the bucket already exists |

---

## 4. Image processing (Huawei OBS `x-image-process` resize)

Append the `x-image-process` query parameter to a **download URL** and the gateway returns a
resized image (thumbnail / preview) on the fly. This is an HTTP-URL-level capability — used via
a **presigned URL or a directly constructed URL** (like a browser `<img src>`), not a native
SDK method.

```
GET http://<gateway-host>:8080/<bucket>/<key>?x-image-process=image/resize,w_200,h_100,m_lfit
```

| Parameter | Description |
|---|---|
| `w_` / `h_` | Target width / height in pixels; **provide at least one**. When only one is given, the other is computed proportionally. Each capped at 4096. |
| `m_` (mode) | `lfit` (default: fit inside the box, keep aspect ratio, no crop); `fill` (cover then center-crop to exactly `w×h`); `fixed` (exactly `w×h`, ignore aspect ratio). Unknown modes fall back to `lfit`. |

- **Only `image/resize` is supported** — not Huawei Cloud's full image-processing API.
- Input: jpeg / png / gif (first frame). Output: png source → png, otherwise → jpeg.
- Objects **> 20 MB** are not processed → `413`; non-images / unparseable → `400`; an
  **unrecognized spec passes the original object through** unchanged.
- Derived images are **not cached** (computed each time) and **do not support Range** (returned
  whole).

```bash
# Resize to width 200, keep aspect ratio
curl "http://<gw>:8080/demo/user/123/avatar.png?x-image-process=image/resize,w_200" -o thumb.png
# 200×200 square (cover and crop)
curl "http://<gw>:8080/demo/user/123/avatar.png?x-image-process=image/resize,w_200,h_200,m_fill" -o square.jpg
```

---

## 5. Caveats (read before integrating)

### 5.1 copyObject is supported (an earlier limitation is fixed)

If you saw an earlier note that "copyObject crashes through the OBS SDK" — **it is fixed and
works now.**

Root cause: the underlying library gofakes3 split the `X-Amz-Copy-Source` header on `/`
**before** url-decoding it, while the OBS SDK percent-encodes that header wholesale (including
the `/`), causing an out-of-bounds panic on the split. The gateway adds a middleware in front
of gofakes3 that normalizes the header into the form gofakes3 expects before passing it
through. Verified with the real OBS SDK (copy → read back → delete source → copy still
present).

### 5.2 No signature verification (write security via network isolation)

The gateway **does not verify S3 signatures** (gofakes3 doesn't check them); any access key
passes. Write/delete security depends entirely on network isolation — **bind to an internal
interface only**. Server-side AK/SK authentication is a second-phase item.

### 5.3 Presigned GET works, but is not access control

A URL from `createSignedUrlSync` returns the object correctly, but because signatures aren't
verified ([§5.2](#52-no-signature-verification-write-security-via-network-isolation)), **the
bare URL without the signature query params returns the object just as well.** A presigned URL
therefore **cannot serve as access control for private objects** (real signature enforcement,
SigV4, is a second-phase item).

### 5.4 Empty (0-byte) objects are rejected

A `PUT` of a 0-byte object returns `InvalidArgument` (400) — 0G cannot address zero-byte
content. Skip empty objects when migrating existing data.

### 5.5 No list pagination

`listObjects` supports `Prefix` filtering but **does not implement pagination**: it returns all
matching items in the bucket at once. Take care with buckets that hold very many objects.

### 5.6 Uploads are asynchronous (PUT return ≠ persisted on-chain)

`putObject` returns success once the object is written to the **local cache**, at which point
it is **immediately readable**; the actual **0G upload** (batch → on-chain → await finality)
runs in the background. **Do not assume data is finalized on 0G the moment `PUT` returns.**

### 5.7 Conditional writes are not atomic

`If-Match` / `If-None-Match` are supported, but the condition check happens **before** the slow
ingest and the key write happens **after** — the two are not atomic. Concurrent conditional
`PUT`s to the same key may both pass. The single-writer demo scenario accepts this.

### 5.8 Metadata is per content, not per key

An object's Content-Type / filename are derived from its **content** (identical bytes
deduplicate to the same underlying object). So two keys with **identical bytes but different
Content-Types** will both report the first writer's Content-Type on GET/HEAD. Identical bytes
usually share a type, so the impact is minimal.

### 5.9 Other

- **No multipart upload** — the library buffers fragments in memory and writes once, so it is
  **only suitable for small files**; a single object must be ≤ `ZGS_GW_MAX_SIZE` (default 4 GiB).
- **No versioning** — an overwrite replaces in place.
- **Deletion = removing the key mapping** — after deletion the object is inaccessible in the
  bucket; 0G data is content-addressed and immutable and **cannot be physically erased**.
  Compliance-grade erasure requires a separate discussion.
- **No bucket-level ACLs / policies / lifecycle rules** or other advanced S3 features.

---

## 6. Verify connectivity

**Command line (`aws s3api`, any access key):**

```bash
export AWS_ACCESS_KEY_ID=anyAK AWS_SECRET_ACCESS_KEY=anySK AWS_DEFAULT_REGION=us-east-1
EP=http://<gateway-host>:8080
aws --endpoint-url $EP s3api create-bucket --bucket demo
aws --endpoint-url $EP s3api put-object  --bucket demo --key user/123/a.png --body ./a.png
aws --endpoint-url $EP s3api get-object  --bucket demo --key user/123/a.png ./out.png
aws --endpoint-url $EP s3api list-objects --bucket demo --prefix user/
```

**Real Huawei OBS Node.js SDK harness (shipped with the repo):**

```bash
cd integration/testdata/obs-js && npm install
go test ./integration/ -run TestOBSJavaScriptSDK -v
```

That script (`obs_sdk_test.js`) is the basis of the [§2](#2-supported-operations-verified-via-the-huawei-obs-sdk)
support matrix and doubles as an integration example.

---

## 7. Migration playbook

Migrating with the OBS SDK is roughly a "**change the endpoint + switch to V2 signing /
path-style**" effort:

1. **Update the connection config** — point the endpoint at the gateway, `signature='v2'` +
   `path_style=true`, any non-empty access key.
2. **Upload / download / delete / list** — keep your existing
   `putObject / getObject / deleteObject / listObjects(prefix)`; bucket names and object keys
   are unchanged.
3. **`copyObject` works** (the earlier limitation is fixed, [§5.1](#51-copyobject-is-supported-an-earlier-limitation-is-fixed)).
4. **Multi-tenancy / partitioning** — use buckets + key prefixes (e.g. bucket `tenantA`, key
   `avatars/123.png`).

**Bulk data migration**

- Walk the source OBS → download each object → `putObject` to the gateway (preserve bucket and
  key names).
- **Naturally re-runnable** — content is deduplicated underneath, so identical content uploads
  once and re-running after an interruption does not re-upload or re-spend gas.
- Remember to **skip 0-byte objects** ([§5.4](#54-empty-0-byte-objects-are-rejected)); each
  object must be ≤ 4 GiB.
- Consider dual-writing for a while (write to both source OBS and the gateway), cut reads over
  gradually, then decommission the source OBS.

**Server-side configuration**

The gateway reads its settings from a config file and/or environment variables (file path via
`--config` / `$ZGS_CONFIG`, default `./config.yaml`); env vars override file values. See the
[README](../README.md#configuration) and `config.example.yaml` for the full reference. The
0G-specific settings are:

| Config key (env var) | Description |
|---|---|
| `nodes` (`ZGS_NODES`) / `eth_rpc` (`ZGS_ETH_RPC`) / `private_key` (`ZGS_PRIVATE_KEY`) | **Required** — 0G storage-node RPCs / host-chain RPC / signer key |
| `listen` (`ZGS_GW_LISTEN`) | S3/OBS endpoint listen address, default `:8080`. ⚠️ no signature check — **bind internal-only** (e.g. `127.0.0.1:8080`) |
| `data_dir` (`ZGS_GW_DATA_DIR`) | Local data / cache directory, default `./data` |
| `max_size` (`ZGS_GW_MAX_SIZE`) | Max object size in bytes, default 4 GiB |
| `batch_max` (`ZGS_GW_BATCH_MAX`) | Max objects per on-chain batch, default 20 |
| `flush_interval_ms` (`ZGS_GW_FLUSH_INTERVAL_MS`) | Worker flush interval in ms, default 3000, must be > 0 |
| `max_retries` (`ZGS_GW_MAX_RETRIES`) | On-chain upload retry ceiling, default 5 |
| `expected_replica` (`ZGS_EXPECTED_REPLICA`) | Desired replica count, default = number of nodes |
