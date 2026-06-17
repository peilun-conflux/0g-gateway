# 0G Storage Gateway — 华为 OBS SDK 对接文档

本文档面向**从华为 OBS 迁移过来的后端服务**,说明如何用**华为 OBS SDK** 直连本网关、
哪些操作已支持、以及**对接时必须注意的地方**。

网关对外是一套 **S3 兼容协议端点**。华为 OBS 协议与 S3 高度兼容,因此 OBS SDK 把 endpoint
指向本网关、并切到 **S3 v2 签名 + 路径风格**后即可直接使用,沿用你原有的 `bucket + objectKey`
编程模型。网关内部把「桶 + 键」映射到 0G 去中心化存储(内容寻址、自动去重),对你透明。

> ⚠️ **当前不校验签名**(demo 级,见 §5)。AK/SK 任意值都能通过,写入安全完全依赖**只绑内网接口**。
> 务必把网关绑在内网地址(如 `127.0.0.1:8080`),**勿绑 `0.0.0.0` / 公网**。

---

## 0. 支持情况速览

| | 状态 |
|---|---|
| 实测 SDK | 华为 OBS **Java** SDK `esdk-obs-java-bundle`(实测 **3.21.11 与 3.25.5**;对接方 Java 8 + ≥3.21.11)与 **Node.js** SDK `esdk-obs-nodejs 3.26`,均有真机测试(`integration/`) |
| 实测覆盖 | 建桶 / 上传 / 下载 / Range / HEAD / 列举 / 复制 / 预签名 GET / 删除 —— 两个 SDK **全部通过** |
| 必须配置 | path-style + S3 v2 签名(见 §1) |
| 不支持 | Multipart 分段上传、空对象、List 分页、Versioning |
| 安全 | **不验签名**(demo),只绑内网;预签名 URL 能取到对象但**不被强制**(非访问控制) |

---

## 1. SDK 连接配置

华为 OBS SDK 默认走 OBS 私有签名协议,需调成 **S3 v2 签名 + 路径风格**。

### 1.1 华为 OBS Java SDK(对接方使用,已真机实测)

`esdk-obs-java-bundle`(实测 3.21.11 与 3.25.5;对接方 Java 8 + ≥3.21.11):

```java
import com.obs.services.ObsClient;
import com.obs.services.ObsConfiguration;
import com.obs.services.model.AuthTypeEnum;

ObsConfiguration config = new ObsConfiguration();
config.setEndPoint("<gateway-host>");      // 仅主机名/IP,不含 scheme
config.setEndpointHttpPort(8080);          // 端口
config.setHttpsOnly(false);                // 内网走 http
config.setPathStyle(true);                 // 必须:路径风格
config.setAuthTypeNegotiation(false);      // 不与服务端协商,固定签名类型
config.setAuthType(AuthTypeEnum.V2);       // 必须:S3 v2 签名(不是 OBS 私有协议)
ObsClient client = new ObsClient("anyAK", "anySK", config); // AK/SK 任意非空值
```

要点:
- **`setPathStyle(true)`** + **`setAuthType(AuthTypeEnum.V2)`** + **`setAuthTypeNegotiation(false)`** 缺一不可。
- **无需** `setVerifyResponseContentType(false)`:网关已在服务端把 XML 响应的 `Content-Type` 规范化为 `application/xml`,SDK 保持**默认严格校验**即可正常用(否则 SDK 会因 gofakes3 的 `text/xml; charset=utf-8` 报「Expected XML document response」)。
- AK/SK 任意非空值——网关不验证签名(见 §5.2)。

> 实测脚本见 `integration/testdata/obs-java/ObsCompatTest.java`(由 `integration/obssdkjava_test.go` 驱动),覆盖 §2 全部操作。

### 1.2 华为 OBS Node.js SDK(已真机实测)

```js
const ObsClient = require('esdk-obs-nodejs');

const client = new ObsClient({
  access_key_id:     'anyAK',                       // 任意非空值;网关不验签名
  secret_access_key: 'anySK',
  server:            'http://<gateway-host>:8080',  // 完整 URL(含 http:// 与端口)
  signature:         'v2',                          // 必须;默认 'obs' 走不通
  path_style:        true,                          // 必须;不支持 virtual-host 风格
});
```
host 为 **IP** 时 SDK 会自动选 path-style 并关闭签名协议协商;用域名时把上面两项显式给上即可。

### 1.3 其它 S3 SDK(AWS SDK / boto3 等)

原理相同:endpoint 指向网关、强制 path-style、AK/SK 任意非空。**未逐一实测**,接入前建议先用 §6 的 `aws s3api` 验证连通性。

---

## 2. 已支持的操作(经华为 OBS SDK 实测通过)

下列调用经真实华为 OBS **Java** 与 **Node.js** SDK 跑通(`integration/testdata/obs-java/ObsCompatTest.java` 与 `…/obs-js/obs_sdk_test.js`):

| OBS SDK 方法 | 行为 / 说明 |
|---|---|
| `createBucket` | 建桶。桶已存在返回 `BucketAlreadyExists`。也可不显式建桶——见 §3 自动建桶 |
| `putObject` | 上传对象。`ContentType` 等会作为元数据保存,下载时回填。**拒绝 0 字节对象**(见 §5.4) |
| `getObject` | 下载对象;**支持 `Range`**(断点续传 / 分片) |
| `getObjectMetadata` | 取对象头(HEAD):大小、Content-Type、ETag,不触发从 0G 回源 |
| `listObjects` | 列举对象,**支持 `Prefix` 前缀过滤**;**不支持分页**(见 §5.5) |
| `deleteObject` | 删除对象(删除后该键 GET 返回 404) |
| `copyObject` | 服务端复制;**零拷贝**(内容寻址,同字节不重传)。中间件规范化 `X-Amz-Copy-Source` 后经 OBS SDK 正常工作(见 §5.1) |
| `createSignedUrlSync`(Method=GET) | 生成预签名下载 URL;能正常取回对象。⚠️ **但不被强制**,见 §5.3 |

> **ETag = 对象内容的 MD5**(PUT 时由网关在摄取时计算)。
> **异步语义**:`putObject` 返回成功即代表已写入本地缓存、**立即可读**;真正写入 0G 由后台异步完成(见 §5.6)。

---

## 3. 已实现、但未逐一经 OBS SDK 实测的操作

下列操作在网关后端已实现并有 Go 单测覆盖,但未在上面的 OBS SDK 实测脚本中逐一驱动。功能应可用,接入前建议自行验证:

| 能力 | OBS SDK 对应(典型) | 说明 |
|---|---|---|
| 列桶 | `listBuckets` | 列出所有桶 |
| 探测桶 | `headBucket` | 桶是否存在 |
| 删桶 | `deleteBucket` | **仅能删空桶**;非空返回 `BucketNotEmpty`,不存在返回 `NoSuchBucket` |
| 批量删对象 | `deleteObjects` | 删不存在的键不报错(S3 语义) |
| 条件写 | `putObject` + `If-Match` / `If-None-Match` | 已支持,但与写入**非原子**(见 §5.7) |
| 自动建桶 | —— | 向**不存在的桶** PUT 会**自动建该桶**,兼容「应用假定桶已存在、不显式建桶」的写法 |

---

## 4. 图片处理(华为 OBS `x-image-process` 缩放)

在**下载 URL** 上追加 `x-image-process` 查询参数,网关实时返回缩放后的图片(缩略图 / 预览)。
这是 HTTP URL 层的能力——一般通过**预签名 URL 或直接拼 URL**(像浏览器 `<img src>`)使用,而非某个原生 SDK 方法。

```
GET http://<gateway-host>:8080/<bucket>/<key>?x-image-process=image/resize,w_200,h_100,m_lfit
```

| 参数 | 说明 |
|---|---|
| `w_` / `h_` | 目标宽 / 高(像素),**至少给一个**;只给一个时按比例算另一边。每个上限 4096 |
| `m_`(缩放模式) | `lfit`(默认,缩到框内、保持比例不裁剪);`fill`(铺满后居中裁剪到精确 `w×h`);`fixed`(精确 `w×h`,忽略原比例)。其它模式回退为 `lfit` |

- **仅支持 `image/resize`**,不是华为云完整图片处理 API。
- 输入:jpeg / png / gif(取首帧);输出:png 源 → png,其它 → jpeg。
- 对象 **> 20MB** 不处理 → `413`;非图片 / 无法解析 → `400`;**无法识别的 spec → 原样返回对象**(透传)。
- 派生图**不缓存**(每次实时算),**不支持 Range**(整体返回)。

```bash
# 缩到宽 200,保持比例
curl "http://<gw>:8080/demo/user/123/avatar.png?x-image-process=image/resize,w_200" -o thumb.png
# 200×200 方图(铺满裁剪)
curl "http://<gw>:8080/demo/user/123/avatar.png?x-image-process=image/resize,w_200,h_200,m_fill" -o square.jpg
```

---

## 5. 注意事项(对接前务必阅读)

### 5.1 copyObject 已支持(早期限制已修复)

如果你看过早期说明称「copyObject 经 OBS SDK 会崩」——**已修复,现在可正常使用。**

根因:底层库 gofakes3 在 url-decode **之前**就按 `/` 切分 `X-Amz-Copy-Source` 头,而 OBS SDK 把该头
整体百分号编码(连 `/` 也编码),导致切分越界 panic。网关在 gofakes3 前加了一个中间件,先把该头
规范化成 gofakes3 认的形式再放行。已用真实 OBS SDK 实测(复制 → 读回 → 删源后副本仍在)通过。

### 5.2 不验证签名(写安全靠内网隔离)

网关**不校验 S3 签名**(gofakes3 不验签),AK/SK 任意值都通过。写入/删除安全完全依赖网络隔离——
**只能绑内网接口**。「服务端 AK/SK 鉴权」为第二阶段。

### 5.3 预签名 GET 能用,但**不是访问控制**

`createSignedUrlSync` 生成的 URL 能正常取回对象,但因为 §5.2 不验签,**去掉签名参数的裸 URL 一样能取到对象**。
所以预签名 URL **不能当作私有对象的访问控制手段**(真正的签名强制是第二阶段的 SigV4)。

### 5.4 不支持空对象(0 字节)

PUT 0 字节对象返回 `InvalidArgument`(400)——0G 无法寻址零字节内容。存量迁移时需跳过空对象。

### 5.5 List 无分页

`listObjects` 支持 `Prefix` 过滤,但**未实现分页**:一次性返回当前桶的匹配项。对象极多的桶需注意。

### 5.6 上传是异步的(PUT 返回 ≠ 已落链)

`putObject` 在对象写入**本地缓存**后即返回成功,此时已**立即可读**;真正的 **0G 上链**由后台异步完成
(攒批 → 上链 → 等最终性)。**不要假设 PUT 刚返回时数据已在 0G 上 finalized。**

### 5.7 条件写非原子

`If-Match` / `If-None-Match` 已支持,但条件检查发生在慢速摄取**之前**、键写入在**之后**,二者非原子。
并发对同一 key 的条件 PUT 可能都通过。demo 单写者场景接受此限制。

### 5.8 元数据按内容、非按 key

对象的 Content-Type / 文件名取自其**内容**(同字节去重到同一底层对象)。因此**字节完全相同、
但 Content-Type 不同**的两个 key,GET/HEAD 会返回首个写入者的 Content-Type。通常同字节同类型,影响极小。

### 5.9 其它

- **无分段上传(Multipart)**:库层用内存攒齐分片后一次写入,**仅适合小文件**;单对象 ≤ `ZGS_GW_MAX_SIZE`(默认 4 GiB)。
- **无版本控制(Versioning)**:覆盖写直接替换。
- **删除 = 移除键映射**:删除后对象在桶内不可访问;0G 数据内容寻址、不可变,**无法物理擦除**;有合规擦除需求需单独讨论。
- **无桶级 ACL / 策略 / 生命周期规则**等高级 S3 特性。

---

## 6. 快速验证连通性

**命令行(`aws s3api`,任意 AK/SK)**:
```bash
export AWS_ACCESS_KEY_ID=anyAK AWS_SECRET_ACCESS_KEY=anySK AWS_DEFAULT_REGION=us-east-1
EP=http://<gateway-host>:8080
aws --endpoint-url $EP s3api create-bucket --bucket demo
aws --endpoint-url $EP s3api put-object  --bucket demo --key user/123/a.png --body ./a.png
aws --endpoint-url $EP s3api get-object  --bucket demo --key user/123/a.png ./out.png
aws --endpoint-url $EP s3api list-objects --bucket demo --prefix user/
```

**真实华为 OBS Node.js SDK 实测脚本**(随仓库提供):
```bash
cd integration/testdata/obs-js && npm install
go test ./integration/ -run TestOBSJavaScriptSDK -v
```
该脚本(`obs_sdk_test.js`)就是 §2 支持矩阵的依据,可直接作为对接示例参考。

---

## 7. 迁移落地建议

用 OBS SDK 迁移基本是「**换 endpoint + 切 v2 签名/路径风格**」级别的改动:

1. **改连接配置**:endpoint 指向网关,`signature='v2'` + `path_style=true`,AK/SK 任意非空。
2. **上传 / 下载 / 删除 / 列举**:沿用原 `putObject / getObject / deleteObject / listObjects(prefix)`,桶名和对象键不变。
3. **`copyObject` 可正常使用**(早期限制已修复,§5.1)。
4. **多租户 / 分区**:用桶 + key 前缀区分(如桶 `tenantA`,键 `avatars/123.png`)。

**存量数据迁移**
- 遍历源 OBS → 逐个下载 → `putObject` 到网关(桶名 / key 原样保留)。
- **天然可重入**:底层内容去重,相同内容只上链一次,中断重跑不会重复上传 / 重复花 gas。
- 注意**跳过 0 字节对象**(§5.4),单对象需 ≤ 4 GiB。
- 建议先双写一段(新写同时进源 OBS 和网关),灰度切读,再下线源 OBS。

**服务端配置(`.env`)**
| 变量 | 说明 |
|---|---|
| `ZGS_GW_LISTEN` | 网关监听地址(对外 S3/OBS 端点),默认 `:8080`。⚠️ 不验签名,**绑内网**(建议 `127.0.0.1:8080`) |
| `ZGS_GW_DATA_DIR` | 本地数据 / 缓存目录,默认 `./data` |
| `ZGS_GW_MAX_SIZE` | 单对象上限(字节),默认 4 GiB |
| `ZGS_GW_BATCH_MAX` | 上链攒批上限,默认 20 |
| `ZGS_GW_FLUSH_INTERVAL_MS` | worker 刷新间隔(毫秒),默认 3000,必须 > 0 |
| `ZGS_GW_MAX_RETRIES` | 上链重试上限,默认 5 |
| `ZGS_EXPECTED_REPLICA` | 期望副本数,默认 = 节点数 |
| `ZGS_NODES` / `ZGS_ETH_RPC` / `ZGS_PRIVATE_KEY` | (必填)0G 存储节点 RPC / 宿主链 RPC / 出证私钥 |
