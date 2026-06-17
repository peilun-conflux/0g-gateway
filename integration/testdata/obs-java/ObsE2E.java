// Live end-to-end client for TestLiveE2ESDK: drives the gateway's S3 endpoint
// with the real Huawei OBS Java SDK over HTTP. Two phases (OBS_PHASE):
//   put → createBucket + putObject(OBS_BODY)
//   get → getObject and assert the body matches OBS_BODY (a cold read from 0G
//         after the Go harness drops the local cache)
// Config mirrors the demo guidance: path-style + Sig V2, negotiation off.
import com.obs.services.ObsClient;
import com.obs.services.ObsConfiguration;
import com.obs.services.exception.ObsException;
import com.obs.services.model.AuthTypeEnum;
import com.obs.services.model.ObjectMetadata;
import com.obs.services.model.ObsObject;

import java.io.ByteArrayInputStream;
import java.io.ByteArrayOutputStream;
import java.io.InputStream;
import java.net.URI;
import java.nio.charset.StandardCharsets;

public class ObsE2E {
    public static void main(String[] args) {
        try {
            String endpoint = System.getenv("OBS_ENDPOINT");
            String bucket = System.getenv("OBS_BUCKET");
            String key = System.getenv("OBS_KEY");
            String body = System.getenv("OBS_BODY");
            String phase = System.getenv("OBS_PHASE");
            URI u = URI.create(endpoint);

            ObsConfiguration config = new ObsConfiguration();
            config.setEndPoint(u.getHost());
            config.setEndpointHttpPort(u.getPort());
            config.setHttpsOnly(false);
            config.setPathStyle(true);
            config.setAuthTypeNegotiation(false);
            config.setAuthType(AuthTypeEnum.V2);
            ObsClient client = new ObsClient("anyAK", "anySK", config);

            byte[] bytes = body.getBytes(StandardCharsets.UTF_8);

            if ("put".equals(phase)) {
                try {
                    client.createBucket(bucket);
                } catch (ObsException e) {
                    // bucket may already exist / auto-create — ignore
                }
                ObjectMetadata md = new ObjectMetadata();
                md.setContentLength((long) bytes.length);
                client.putObject(bucket, key, new ByteArrayInputStream(bytes), md);
                System.out.println("PUT ok (" + bytes.length + " bytes)");
            } else if ("get".equals(phase)) {
                ObsObject o = client.getObject(bucket, key);
                ByteArrayOutputStream out = new ByteArrayOutputStream();
                byte[] buf = new byte[8192];
                int n;
                InputStream in = o.getObjectContent();
                while ((n = in.read(buf)) != -1) {
                    out.write(buf, 0, n);
                }
                in.close();
                String got = new String(out.toByteArray(), StandardCharsets.UTF_8);
                if (!got.equals(body)) {
                    throw new RuntimeException("GET body mismatch: got " + got.length() + " bytes, want " + bytes.length);
                }
                System.out.println("GET ok (" + got.length() + " bytes, cold-read from 0G, match)");
            } else {
                throw new RuntimeException("unknown OBS_PHASE: " + phase);
            }
            client.close();
            System.exit(0);
        } catch (Throwable t) {
            System.out.println("FAIL");
            t.printStackTrace();
            System.exit(1);
        }
    }
}
