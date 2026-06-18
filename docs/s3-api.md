# S3 API reference

A concise summary of what the gateway supports and how it differs from Huawei OBS. The gateway
provides **standard S3-compatible** object storage and can replace Huawei OBS: point **any S3
SDK or the Huawei OBS SDK** at it, keep reading and writing with your existing bucket names and
object keys, and the change is minimal. For the full integration guide, see
[migration-from-obs.md](migration-from-obs.md).

## 1. Connecting

- **Endpoint:** `http://<gateway-host>:8080`
- **Protocol:** S3, **path-style**
- **Access key:** any non-empty value (signatures are not verified — see [§3](#3-differences-from-huawei-obs))

The Huawei OBS **Java** SDK (`esdk-obs-java-bundle`, verified) is configured like this:

```java
ObsConfiguration config = new ObsConfiguration();
config.setEndPoint("<gateway-host>");    // hostname/IP only
config.setEndpointHttpPort(8080);
config.setHttpsOnly(false);              // plain HTTP on an internal network
config.setPathStyle(true);               // required: path-style
config.setAuthTypeNegotiation(false);
config.setAuthType(AuthTypeEnum.V2);     // required: S3 V2 signing
ObsClient client = new ObsClient("anyAK", "anySK", config); // AK/SK: any non-empty value
```

> You do **not** need `setVerifyResponseContentType(false)` — the gateway normalizes XML
> responses server-side, so the SDK's default strict verification works.
> The Node.js SDK is analogous (`signature:'v2'` + `path_style:true` + a full-URL `server`);
> the standard AWS SDK / boto3 just need path-style forced.

## 2. Supported operations

| Operation | Notes |
|---|---|
| PutObject | Body is the file content; bucket + key as in OBS. Supports conditional writes (If-Match / If-None-Match). |
| GetObject | Supports Range (resumable / chunked download). |
| HeadObject | Metadata only (size, Content-Type, ETag). |
| DeleteObject / bulk delete | Deleting a missing key is not an error (S3 semantics). |
| Overwrite | Re-`PUT` the same key to overwrite. |
| CopyObject | Zero-copy (identical content is not re-uploaded); verified through the Huawei OBS SDK. |
| ListObjects | Supports prefix filtering (**no pagination**). |
| Bucket operations | create / list / probe / delete-empty; a write to a non-existent bucket **auto-creates** it. |
| Image resize preview | `GET .../<bucket>/<key>?x-image-process=image/resize,w_200,h_100,m_lfit` — on-the-fly thumbnail (resize only). |

An object's filename and Content-Type are stored as metadata and returned on download; ETag =
content MD5.

> Example: `putObject(bucket, "user/123/avatar.png", ...)` to upload, `getObject(...)` to
> download — essentially identical to OBS usage.

## 3. Differences from Huawei OBS

| # | Difference | Notes |
|---|---|---|
| 1 | **No signature verification** | S3 signatures are not checked; any access key passes. Write security relies on **binding internal-only** — never expose the endpoint publicly. Server-side AK/SK authentication is a future item. |
| 2 | **No multipart upload** | Multipart Upload is unsupported (the library buffers fragments in memory — small files only); single object capped at 4 GiB. |
| 3 | **No empty files** | 0-byte objects are rejected (returns 400). |
| 4 | **No list pagination** | List supports prefix filtering but not pagination; no versioning. |
| 5 | **Asynchronous upload** | `PUT` returns readable immediately (cached locally); the actual write to 0G completes asynchronously — don't assume `PUT` return means permanently persisted on-chain. |
| 6 | **Deletion = removing the key mapping** | After deletion the object is inaccessible in the bucket; 0G data is content-addressed and cannot be physically erased — compliance-grade erasure needs a separate discussion. |

The most common upload / download / delete / overwrite / list operations are all supported. For
the detailed integration reference, see [migration-from-obs.md](migration-from-obs.md).
