# 0G Storage Gateway 接口文档（华为 OBS / S3 SDK 对接）

本文档面向**后端服务**，描述如何把原来对接华为 OBS 的存储逻辑迁移到本 Gateway。

本 Gateway **只提供一套 S3 兼容协议端点**：你用**任意 S3 SDK**或**华为 OBS SDK**把 endpoint 指向本网关即可，不需要改你既有的 bucket + objectKey 编程模型。网关内部把 S3 的「桶 + 键」映射到 0G 去中心化存储（内容寻址、自动去重），对你透明。

- Endpoint（S3 服务地址）：`http://<gateway-host>:8080`（默认监听 `:8080`，由 `ZGS_GW_LISTEN` 配置）
- 协议：S3（路径风格 path-style）。建议前置 Nginx/网关做 TLS 与限流
- 寻址：`bucket` + `objectKey`（沿用你原 OBS 的桶名和对象键，可含 `/`）

> ⚠️ **当前不校验签名**（demo 级，见 §6）。AK/SK 随便填都能通过，写安全完全依赖**只绑内网接口**。务必把网关绑在内网地址（如 `127.0.0.1:8080`），勿绑 `0.0.0.0` / 公网。

---

## 1. SDK 连接配置

### 1.1 华为 OBS Node.js SDK（`esdk-obs-nodejs`，已实测打通）

华为 OBS SDK 默认走 OBS 私有签名协议，需要调成 S3 v2 + 路径风格：

```js
const ObsClient = require('esdk-obs-nodejs');

const client = new ObsClient({
  access_key_id:     'anyAK',                 // 任意非空值；网关不验签名
  secret_access_key: 'anySK',                 // 任意非空值
  server:            'http://<gateway-host>:8080', // 完整 URL（含 http://）
  signature:         'v2',                    // 必须；默认 'obs' 走不通
  path_style:        true,                    // 必须；不支持 virtual-host 风格
});
```

关键点（缺一不可）：
- **`signature: 'v2'`** —— 不是默认的 `'obs'`。
- **`path_style: true`** —— 网关只支持路径风格（`http://host/bucket/key`），不支持 `http://bucket.host/key`。
- **`server` 传完整 URL**（含 `http://` 与端口）。当 host 是 **IP** 时，SDK 会自动选 path-style 并关闭签名协议协商；用域名时上面两项显式给上即可。
- **AK/SK 任意非空值**即可 —— 网关**不验证签名**（见 §6）。

### 1.2 其它 S3 SDK（AWS SDK / boto3 等）

任何标准 S3 SDK 都能接：把 endpoint 指向网关、强制 path-style、AK/SK 随便给。例（Python boto3）：

```python
import boto3
s3 = boto3.client(
    's3',
    endpoint_url='http://<gateway-host>:8080',
    aws_access_key_id='anyAK',
    aws_secret_access_key='anySK',
    config=boto3.session.Config(s3={'addressing_style': 'path'}),
    region_name='us-east-1',  # 任意值
)
s3.put_object(Bucket='demo', Key='user/123/avatar.png', Body=open('avatar.png','rb'))
```

### 1.3 命令行快速验证（`aws s3api`）

```bash
export AWS_ACCESS_KEY_ID=anyAK AWS_SECRET_ACCESS_KEY=anySK AWS_DEFAULT_REGION=us-east-1
EP=http://<gateway-host>:8080

aws --endpoint-url $EP s3api create-bucket --bucket demo
aws --endpoint-url $EP s3api put-object  --bucket demo --key user/123/avatar.png --body ./avatar.png
aws --endpoint-url $EP s3api get-object  --bucket demo --key user/123/avatar.png ./out.png
aws --endpoint-url $EP s3api list-objects --bucket demo --prefix user/
```

---

## 2. 支持的 S3 操作

下表为本网关已实现的 S3 操作及其行为差异（未列出的 S3 操作一律视为不支持）。

### 2.1 桶操作（Bucket）

| 操作 | 行为 |
|---|---|
| CreateBucket | 创建桶；桶已存在返回 `BucketAlreadyExists` |
| ListBuckets | 列出所有桶 |
| HeadBucket / BucketExists | 探测桶是否存在 |
| DeleteBucket | **仅能删空桶**；非空桶返回 `BucketNotEmpty`；桶不存在返回 `NoSuchBucket` |

> **自动建桶（Auto-Bucket）已开启**：向**不存在的桶** PUT 对象时会**自动创建该桶**（`WithAutoBucket(true)`），兼容「应用假定桶已存在、不显式建桶」的写法。

### 2.2 对象操作（Object）

| 操作 | 行为 |
|---|---|
| PutObject | 原始 body 上传。**支持条件写**（`If-Match` / `If-None-Match`）。**拒绝 0 字节对象**（0G 无法寻址零字节）→ `InvalidArgument`（400）。超大小上限 → `InvalidArgument` |
| GetObject | 下载；**支持 `Range`**（断点续传 / 分片） |
| HeadObject | 仅取元数据（大小、Content-Type、ETag），不触发从 0G 回源 |
| DeleteObject | 删除单个键。**删除不存在的键不报错**（S3 语义） |
| DeleteObjects（批量） | 批量删除；同样删除不存在的键不报错 |
| CopyObject | **零拷贝**（内容寻址：同字节 ⇒ 同 root，不重新上传、不搬运字节），只重映射目标键。⚠️ 经 **OBS SDK** 调用有已知坑，见 §5 |
| ListObjects（ListBucket） | **支持 `prefix` 前缀过滤**；**不支持分页**（无 pagination，见 §5） |

**ETag = 内容的 MD5。** GET/HEAD/List 返回的 ETag 即对象内容的 MD5（PUT 时由 S3 层自动计算）。

```bash
# 条件写：仅当目标不存在时才写（避免覆盖）
aws --endpoint-url $EP s3api put-object --bucket demo --key a.txt --body a.txt \
  --if-none-match '*'
```

---

## 3. 图片处理（华为 OBS x-image-process 缩放）

在 GET 对象的 URL 上追加 `x-image-process` 查询参数，服务端实时返回缩放后的图片（缩略图 / 预览用）。**仅支持 `image/resize`**，不是华为云完整图片处理 API。

```
GET http://<gw>:8080/<bucket>/<key>?x-image-process=image/resize,w_200,h_100,m_lfit
```

| 参数 | 说明 |
|---|---|
| `w_` / `h_` | 目标宽 / 高（像素），**至少给一个**；只给一个时按比例算另一边。每个上限 4096 |
| `m_`（缩放模式） | `lfit`（默认，缩到框内、保持比例不裁剪）；`fill`（铺满后居中裁剪到精确 `w×h`）；`fixed`（精确 `w×h`，忽略原比例）。其它模式回退为 `lfit` |

- **输入**：jpeg / png / gif（gif 取首帧）。**输出**：png 源 → png，其它 → jpeg。
- 对象 **> 20MB** 不处理 → `413`；非图片 / 无法解析 → `400`；**无法识别的 spec → 原样返回对象**（透传，不报错）。
- 派生图**不缓存**（每次实时算），且**不支持 Range**（整体返回）。

```bash
# 把头像缩到宽 200，保持比例（<img src> 直接用）
curl "http://<gw>:8080/demo/user/123/avatar.png?x-image-process=image/resize,w_200" -o thumb.png
# 200×200 方图（铺满裁剪）
curl "http://<gw>:8080/demo/user/123/avatar.png?x-image-process=image/resize,w_200,h_200,m_fill" -o square.jpg
```

> 注：图片处理是 HTTP 层中间件（在 S3 处理之前拦截 `x-image-process`），对所有桶 / 键生效。

---

## 4. 对象生命周期与异步语义（重要）

**PUT 返回成功 ≠ 已上链。** 网关上传是异步的：

- `PutObject` 在对象**写入本地缓存**后即返回成功，此时对象**已可立即读取**（GET / HEAD / List 均可见）。
- 真正的 **0G 上链**由后台 worker 异步完成（攒批 → 上链 → 等待最终性）。
- **不要假设 PUT 刚返回时数据已在 0G 上 finalized。** 若业务对「已落链」有强一致要求，需自行评估（当前 S3 协议无暴露上链状态的标准字段）。

**删除是「移除桶内键映射」。** 0G 上的数据内容寻址、不可变，**无法物理擦除**；DeleteObject 只是让该键在桶里不可见。若有「彻底物理擦除」的合规要求，需单独讨论。

---

## 5. 已知限制（demo 级，需明确告知对接方）

1. **不验证签名**：网关**不校验 S3 签名**（gofakes3 不验签），AK/SK 任意值都通过。写安全完全依赖网络隔离 —— **只能绑内网接口**。「服务端 AK/SK 写鉴权」为后续阶段。
2. **无分段上传后端（Multipart）**：gofakes3 用内存 fallback 攒齐所有分片后调一次 PutObject —— **仅适合小文件**。单对象 ≤ `ZGS_GW_MAX_SIZE`（默认 4 GiB）。大文件 multipart 为第二阶段。
3. **不支持空对象（0 字节）**：PUT 0 字节返回 `InvalidArgument`（400）。0G 无法寻址零字节。
4. **List 无分页**：`ListObjects` 支持 `prefix` 过滤，但**未实现分页**（pagination）。大量对象时一次性返回当前桶匹配项。
5. **CopyObject 经 OBS SDK 调用会触发 panic**：这是 **gofakes3 库层**的 bug —— 它在 url-decode **之前**就按 `/` 切分 URL 编码的 `X-Amz-Copy-Source` 头。**我们自己的 CopyObject 逻辑是正确的**（Go 测试覆盖，零拷贝），用其它 S3 客户端（如未编码 copy-source 的场景）可正常工作。修复留待后续阶段（自控 S3 层 / 迁移 / 给 gofakes3 打补丁）。
6. **无版本控制（Versioning）**：覆盖写直接替换。
7. **无桶级 ACL / 策略 / 生命周期规则等高级 S3 特性**：仅实现上述核心对象 / 桶操作。

> 第二阶段规划：大文件 multipart、真签名校验（SigV4）、完整 S3 语义（评估迁移到 versitygw 或自控 S3 层）。

---

## 6. 迁移落地建议

用 S3/OBS SDK 后，迁移基本是「**换 endpoint + 调签名/路径风格**」级别的改动：

1. **改连接配置**：endpoint 指向网关，`signature=v2` + `path_style=true`（OBS SDK）或 path-style（标准 S3 SDK），AK/SK 任意非空。
2. **上传 / 下载 / 删除 / 列举**：沿用原 `putObject / getObject / deleteObject / listObjects(prefix)`，桶名和对象键不变。
3. **覆盖写**：重新 PUT 同 key 即覆盖（无版本控制）。
4. **多租户 / 分区**：用桶 + key 前缀区分（如桶 `tenantA`，键 `avatars/123.png`）。

**存量数据迁移**
- 遍历 OBS → 逐个下载 → PutObject 到 Gateway（桶名 / key 原样保留）。
- **天然可重入**：底层内容去重，相同内容只上链一次，中断重跑不会重复上传 / 重复花 gas。
- 建议先双写一段时间（新写同时进 OBS 和 Gateway），灰度切读，再下线 OBS。
- 注意：**跳过 0 字节对象**（网关会拒绝），单对象需 ≤ 4 GiB。

**配置项参考（服务端 `.env`）**
| 变量 | 说明 |
|---|---|
| `ZGS_GW_LISTEN` | S3 端点监听地址，默认 `:8080`。⚠️ 不验签名，**绑内网**（建议 `127.0.0.1:8080`） |
| `ZGS_GW_DATA_DIR` | 本地数据 / 缓存目录，默认 `./data` |
| `ZGS_GW_MAX_SIZE` | 单对象上限（字节），默认 4 GiB |
| `ZGS_GW_BATCH_MAX` | 上链攒批上限，默认 20 |
| `ZGS_GW_FLUSH_INTERVAL_MS` | worker 刷新间隔（毫秒），默认 3000，必须 > 0 |
| `ZGS_GW_MAX_RETRIES` | 上链重试上限，默认 5 |
| `ZGS_EXPECTED_REPLICA` | 期望副本数，默认 = 节点数 |
| `ZGS_NODES` / `ZGS_ETH_RPC` / `ZGS_PRIVATE_KEY` | （必填）0G 存储节点 RPC / 宿主链 RPC / 出证私钥 |
