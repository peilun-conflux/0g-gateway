'use strict';
// Validates the gateway's S3-compatible endpoint (gofakes3 + s3gw) against the
// real Huawei OBS Node.js SDK (esdk-obs-nodejs). The Go harness in
// integration/obssdk_test.go starts the server and runs this script.
//
// Config mirrors the demo guidance: signature=v2 + path_style. Passing a full
// http:// URL as `server` makes the SDK use http and (for an IP host) path-
// style with signature negotiation disabled.
const assert = require('assert');
const ObsClient = require('esdk-obs-nodejs');

const endpoint = process.env.OBS_ENDPOINT || 'http://127.0.0.1:9000';
const bucket = process.env.OBS_BUCKET || 'demo';

const client = new ObsClient({
  access_key_id: process.env.OBS_AK || 'demoAK',
  secret_access_key: process.env.OBS_SK || 'demoSK',
  server: endpoint,
  signature: 'v2',
  path_style: true,
});

function ok(r, step) {
  const m = (r && r.CommonMsg) || {};
  if (!(m.Status >= 200 && m.Status < 300)) {
    throw new Error(`${step} failed: status=${m.Status} code=${m.Code} message=${m.Message}`);
  }
  return r;
}

function bodyText(r) {
  const c = r.InterfaceResult && r.InterfaceResult.Content;
  return Buffer.isBuffer(c) ? c.toString('utf8') : String(c == null ? '' : c);
}

(async () => {
  const key = 'docs/hello.txt';
  const body = 'hello from the Huawei OBS Node.js SDK';

  ok(await client.createBucket({ Bucket: bucket }), 'createBucket');

  ok(await client.putObject({ Bucket: bucket, Key: key, Body: body, ContentType: 'text/plain' }), 'putObject');

  const got = ok(await client.getObject({ Bucket: bucket, Key: key }), 'getObject');
  assert.strictEqual(bodyText(got), body, 'getObject body mismatch');

  const head = ok(await client.getObjectMetadata({ Bucket: bucket, Key: key }), 'getObjectMetadata');
  assert.strictEqual(Number(head.InterfaceResult.ContentLength), Buffer.byteLength(body), 'head content-length mismatch');

  const list = ok(await client.listObjects({ Bucket: bucket, Prefix: 'docs/' }), 'listObjects');
  const keys = (list.InterfaceResult.Contents || []).map((c) => c.Key);
  assert.ok(keys.includes(key), `listObjects missing key; got ${JSON.stringify(keys)}`);

  // NOTE: copyObject via the OBS SDK is intentionally not exercised here.
  // gofakes3 panics parsing the SDK's URL-encoded X-Amz-Copy-Source
  // (splits on "/" before decoding). Our Backend.CopyObject itself is correct
  // (covered by the Go test); this is a gofakes3-layer limitation. See CLAUDE.md.

  // Range request
  const ranged = ok(await client.getObject({ Bucket: bucket, Key: key, Range: 'bytes=0-4' }), 'getObject(range)');
  assert.strictEqual(bodyText(ranged), body.slice(0, 5), 'range body mismatch');

  // Temporary signed URL (presigned GET): the SDK signs the URL; we fetch it
  // with a plain HTTP client (no SDK creds), exactly like a browser <img>/<a>.
  // NOTE: the demo gofakes3 endpoint does NOT verify the signature, so this
  // proves the presigned URL *resolves and returns the object* — not signature
  // enforcement. Real enforcement (phase-2 SigV4 on the S3 endpoint) does not
  // exist yet; until then the negative control below holds.
  const signed = client.createSignedUrlSync({ Method: 'GET', Bucket: bucket, Key: key, Expires: 300 });
  const res = await fetch(signed.SignedUrl);
  assert.strictEqual(res.status, 200, `presigned GET status ${res.status} (url=${signed.SignedUrl})`);
  assert.strictEqual(await res.text(), body, 'presigned GET body mismatch');

  // Negative control: the same object served with NO signature at all. This
  // locks in the fact that the demo S3 endpoint is UNAUTHENTICATED — a 200 here
  // means the presigned URL above conferred no real protection. If a future
  // change adds SigV4 verification, this assertion must be updated (it should
  // then be 403), which is the point: it can't silently start "enforcing".
  const bare = await fetch(signed.SignedUrl.split('?')[0]);
  assert.strictEqual(bare.status, 200, `unauthenticated demo endpoint should serve without a signature, got ${bare.status}`);

  ok(await client.deleteObject({ Bucket: bucket, Key: key }), 'deleteObject');
  const after = await client.getObject({ Bucket: bucket, Key: key });
  assert.strictEqual(after.CommonMsg.Status, 404, `expected 404 after delete, got ${after.CommonMsg.Status}`);

  console.log('OBS JS SDK compatibility: PASS');
  process.exit(0);
})().catch((err) => {
  console.error('OBS JS SDK compatibility: FAIL');
  console.error(err && err.stack ? err.stack : err);
  process.exit(1);
});
