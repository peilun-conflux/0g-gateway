// Validates the gateway's S3-compatible endpoint (gofakes3 + s3gw) against the
// real Huawei OBS *Java* SDK (esdk-obs-java-bundle), which is what the
// integration partner uses (Java 8 + ObsClient). The Go harness in
// integration/obssdkjava_test.go starts the server and runs this program.
//
// Config mirrors the demo guidance: path-style + Sig V2, auth-type negotiation
// disabled so the SDK does not probe for an OBS-native endpoint.
import com.obs.services.ObsClient;
import com.obs.services.ObsConfiguration;
import com.obs.services.exception.ObsException;
import com.obs.services.model.AuthTypeEnum;
import com.obs.services.model.GetObjectRequest;
import com.obs.services.model.HttpMethodEnum;
import com.obs.services.model.ListObjectsRequest;
import com.obs.services.model.ObjectListing;
import com.obs.services.model.ObjectMetadata;
import com.obs.services.model.ObsObject;
import com.obs.services.model.TemporarySignatureRequest;

import java.io.ByteArrayInputStream;
import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.net.HttpURLConnection;
import java.net.URI;
import java.net.URL;
import java.nio.charset.StandardCharsets;

public class ObsCompatTest {
    public static void main(String[] args) {
        try {
            String endpoint = env("OBS_ENDPOINT", "http://127.0.0.1:9000");
            String bucket = env("OBS_BUCKET", "demo");
            String ak = env("OBS_AK", "demoAK");
            String sk = env("OBS_SK", "demoSK");
            URI u = URI.create(endpoint);

            ObsConfiguration config = new ObsConfiguration();
            config.setEndPoint(u.getHost());
            config.setEndpointHttpPort(u.getPort());
            config.setHttpsOnly(false);
            config.setPathStyle(true);            // required: path-style addressing
            config.setAuthTypeNegotiation(false); // don't probe; use a fixed signature type
            config.setAuthType(AuthTypeEnum.V2);  // required: S3 v2 signature (not OBS-native)
            // NOTE: deliberately leaving setVerifyResponseContentType at its strict
            // default (true). The gateway's FixXMLContentTypeHandler normalizes XML
            // responses to application/xml so the default-configured SDK works.
            ObsClient client = new ObsClient(ak, sk, config);

            String key = "docs/hello.txt";
            String body = "hello from the Huawei OBS Java SDK";
            byte[] bytes = body.getBytes(StandardCharsets.UTF_8);

            client.createBucket(bucket);

            ObjectMetadata pm = new ObjectMetadata();
            pm.setContentType("text/plain");
            pm.setContentLength((long) bytes.length);
            client.putObject(bucket, key, new ByteArrayInputStream(bytes), pm);

            ObsObject got = client.getObject(bucket, key);
            expect(readAll(got.getObjectContent()).equals(body), "getObject body mismatch");

            ObjectMetadata head = client.getObjectMetadata(bucket, key);
            expect(head.getContentLength() == bytes.length, "head content-length mismatch");

            ListObjectsRequest lr = new ListObjectsRequest(bucket);
            lr.setPrefix("docs/");
            ObjectListing listing = client.listObjects(lr);
            boolean found = false;
            for (ObsObject o : listing.getObjects()) {
                if (o.getObjectKey().equals(key)) {
                    found = true;
                }
            }
            expect(found, "listObjects missing key");

            // copyObject: the operation that needed the X-Amz-Copy-Source middleware fix.
            String copyKey = "docs/hello-copy.txt";
            client.copyObject(bucket, key, bucket, copyKey);
            ObsObject copied = client.getObject(bucket, copyKey);
            expect(readAll(copied.getObjectContent()).equals(body), "copied object body mismatch");

            // Range request.
            GetObjectRequest gr = new GetObjectRequest(bucket, key);
            gr.setRangeStart(0L);
            gr.setRangeEnd(4L);
            ObsObject ranged = client.getObject(gr);
            expect(readAll(ranged.getObjectContent()).equals(body.substring(0, 5)), "range body mismatch");

            // Presigned GET: the SDK signs the URL; we fetch it with a plain HTTP
            // client. The demo endpoint does NOT verify the signature, so this only
            // proves the URL resolves and returns the object (see the negative control).
            TemporarySignatureRequest tsr = new TemporarySignatureRequest(HttpMethodEnum.GET, bucket, key, null, 300);
            String signedUrl = client.createTemporarySignature(tsr).getSignedUrl();
            expect(httpGet(signedUrl).equals(body), "presigned GET body mismatch");
            // Negative control: the unsigned bare URL also returns the object —
            // locks in that the demo endpoint is unauthenticated.
            String bare = signedUrl.split("\\?")[0];
            expect(httpGet(bare).equals(body), "unauthenticated bare GET should return the object");

            client.deleteObject(bucket, key);
            int status = 0;
            try {
                client.getObject(bucket, key);
            } catch (ObsException e) {
                status = e.getResponseCode();
            }
            expect(status == 404, "expected 404 after delete, got " + status);

            // The copy is an independent key; deleting the source leaves it intact.
            ObsObject copyAfter = client.getObject(bucket, copyKey);
            expect(readAll(copyAfter.getObjectContent()).equals(body), "copy lost after source delete");

            client.close();
            System.out.println("OBS JAVA SDK compatibility: PASS");
            System.exit(0);
        } catch (Throwable t) {
            System.out.println("OBS JAVA SDK compatibility: FAIL");
            t.printStackTrace();
            System.exit(1);
        }
    }

    static String env(String k, String def) {
        String v = System.getenv(k);
        return (v == null || v.isEmpty()) ? def : v;
    }

    static void expect(boolean cond, String msg) {
        if (!cond) {
            throw new RuntimeException(msg);
        }
    }

    static String readAll(InputStream in) throws IOException {
        ByteArrayOutputStream b = new ByteArrayOutputStream();
        byte[] buf = new byte[4096];
        int n;
        while ((n = in.read(buf)) != -1) {
            b.write(buf, 0, n);
        }
        in.close();
        return new String(b.toByteArray(), StandardCharsets.UTF_8);
    }

    static String httpGet(String url) throws IOException {
        HttpURLConnection c = (HttpURLConnection) new URL(url).openConnection();
        c.setRequestMethod("GET");
        int code = c.getResponseCode();
        if (code != 200) {
            throw new IOException("GET " + url + " -> HTTP " + code);
        }
        return readAll(c.getInputStream());
    }
}
