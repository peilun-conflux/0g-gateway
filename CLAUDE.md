# CLAUDE.md — 0G Storage Gateway 维护手册（给 AI agent）

本文件是后续 agent 维护本仓库的权威说明。开工前通读；改代码前先看 §8（约定）和 §9（坑）。

## 1. 这是什么

一个 **S3 兼容的对象存储网关**:对外只说 S3 协议(华为 OBS SDK 等 S3 客户端直连),数据落到
**0G 去中心化存储**。对调用方屏蔽 0G 细节,把它当 OBS/S3 端点用即可。纯 Go,无外部数据库
(元数据用内嵌 bbolt)。

寻址:对外是 **S3 桶 + 对象 key**;网关内部是**内容寻址**——对象的真实 key 是内容的 merkle
root(`0x…`,服务端生成,天然去重 + 可校验)。桶/键 → root 的映射由网关维护,对客户端透明。

> **历史**:早期还有一套原生 HTTP API(`/objects` 内容寻址 + `/kv` key 寻址 + HMAC 签名 URL)。
> 因「S3 是唯一接入方式」,这套原生 API 连同 `internal/server`、`internal/auth` 已整体删除。
> 现在唯一对外接口是 `internal/s3gw`(见 §4.5)。

## 2. 包结构与职责

| 包 | 文件 | 职责 |
|---|---|---|
| `cmd/gateway` | `main.go` | 进程入口:读 env 配置、装配各组件、起 S3 server 与后台 worker、优雅退出 |
| `internal/s3gw` | `backend.go`、`imageprocess.go`、`copysource.go`、`xmlcontenttype.go` | **唯一对外接口**:实现 `gofakes3.Backend`,把 S3 桶+键操作映射到 `object.Service`+`store`;`Wrap` 在 gofakes3 前套三个中间件——`copysource.go`(规范化 `X-Amz-Copy-Source`)、`imageprocess.go`(华为 `?x-image-process=image/resize,...`)、`xmlcontenttype.go`(XML 响应 Content-Type 改 `application/xml`,为 OBS Java SDK)|
| `internal/object` | `service.go` | 摄取/读取管线:spool→sha256(+md5)→去重→merkle root→落缓存→写元数据;冷读回源 |
| `internal/store` | `store.go` | bbolt 持久化:对象元数据、SHA 索引、两条上传队列、桶注册表 + 桶/键→root 索引;每次状态变更一个事务 |
| `internal/uploader` | `worker.go` | 后台批量上传 worker:攒批、对账(reconcile)、重试、最终性轮询 |
| `internal/chain` | `backend.go` | 真实 0G 后端:`BatchUpload` / `FileStatus` / `Download`,封装 0g-storage-client SDK |
| `internal/imageproc` | `imageproc.go` | `image/resize`(纯标准库:decode→bilinear→encode);被 `s3gw` 图片中间件调用 |
| `integration` | `e2e_test.go`、`obssdk_test.go` | 打真网的端到端测试(`ZGS_E2E=1`)+ 真实华为 OBS SDK 兼容测试 |

依赖方向:`s3gw → object → store`;`uploader → store`(+ `Chain` 接口);`chain` 实现 `uploader.Chain` 并被 `main` 注入。**`uploader` 不依赖 `chain` 包**(靠接口解耦,便于 fake 测试)。

## 3. 数据模型(bbolt,单文件 `data/meta.db`)

6 个 bucket:
- `objects`:`root → ObjectMeta(JSON)` —— 唯一真相源
- `sha256`:`sha256(明文) → root` —— 去重索引
- `q_upload`:待上传队列(status = pending|submitted 的 root)
- `q_finalize`:待最终化队列(status = onchain 的 root)
- `s3_buckets`:`桶名 → BucketRecord(JSON)` —— S3 桶注册表
- `s3_keys`:`"bucket/key" → root` —— S3 对象索引(纯索引,不碰对象/队列)

`ObjectMeta` 关键字段:`Root, SHA256, MD5, Size, Filename, ContentType, Status, TxHash, FailReason, Retries, SkipTx, CreatedAt, UpdatedAt`。存 JSON,加字段向后兼容。

**状态机**(`store.Status`):
```
pending ──► submitted ──► onchain ──► finalized
（已缓存，  （已交链后端  （tx 已打包，   （存储节点确认，
  待上传）    待回执）      待最终性）       终态·成功）
   └────────────────────────────────► failed（重试耗尽 / 被 prune，终态）
```
正交标志:`SkipTx`(对账判定 entry 已在链上,只补传分片)。无「逻辑删除」标志——对象一旦摄取即不可删(删除只发生在 S3 索引层,见 §4.4 / §9)。

队列成员关系由 `store.requeue` 统一维护:清空两条队列后按 status 重新入队。`mutate`/`CreateObject` 每次落库都调 `requeue` 保持队列与 status 一致。

## 4. 控制流

### 4.1 摄取 `PUT`(`object.Service.Put`)
1. 流式写 spool 临时文件,同时算 SHA256 + MD5(可选 `MaxSize` 用 `LimitReader` 限流)。
2. 空对象拒绝(`ErrEmpty`);超限拒绝(`ErrTooLarge`)。
3. **按明文 SHA 去重**:命中 → 直接返回(dedup)。其中:
   - 命中对象非 finalized 但**缓存文件丢失** → 用刚写的 spool 救回缓存并重新入队(salvage)。
   - 命中对象是 `failed` → `Reenqueue` 重试。
4. 未命中:`fsync` spool → 算 merkle root(`core.MerkleRoot`)→ `rename` 进缓存 `objects/{root}`。
5. `CreateObject` 写元数据 + SHA 索引 + 入 `q_upload`。
   - 若 `ErrExists`(与并发相同 PUT 抢输了)→ 当 dedup 命中返回。

**崩溃一致性**:缓存文件先 fsync+rename 落地,**再**提交元数据/任务记录。中途崩溃只会留下孤儿缓存文件(无害),不会出现「有任务无数据」。

### 4.2 上传 worker(`uploader.Worker`,后台单 goroutine,`Run` 按 `flushInterval` 循环)
`Flush`:快照 `q_upload` → 按 `BatchMax` 分批 → `processBatch`:
- 逐项**重新读库**(快照可能过期),`Deleted` 的跳过(绝不上传)。
- `submitted`(崩溃残留)或 `Retries>0` 的项,先 `FileStatus` 对账:已 finalized→直接收尾;在链上→置 `SkipTx`;不在链上→清 `SkipTx`。
- 全部置 `submitted` → `ch.BatchUpload`(一次链上 tx 提交整批)。
  - 成功:置 `onchain`;**SkipTx 项保留原 txHash**(传空,不被本批 hash 覆盖,见 §9)。
  - 失败:`reconcileFailedBatch` 逐项对账,涨 `Retries`,超过 `MaxRetries` 置 `failed`,否则回 `pending`。

`PollFinality`:扫 `q_finalize` → `FileStatus`:finalized→`finalized`;pruned→`failed`(告警条件)。

### 4.3 下载 `GET`(`object.Service.Open`)
1. 元数据不存在→`ErrNotFound`。
2. **热路径**:缓存文件存在且**大小等于** `meta.Size` → 直接返回(`http.ServeContent` 处理 Range/HEAD/If-Modified-Since)。大小不符视为损坏,丢弃走冷路径。
3. **冷路径**:从 0G `Download`(**带 merkle proof 校验**)到唯一临时路径 → `rename` 进缓存 → 返回。
   - 缓存损坏但对象不在 0G(如 pending)→ 冷读失败 → `502`(失败关闭,不返回错误字节)。
4. **图片预览**:`s3gw.ImageProcessHandler` 中间件(套在 gofakes3 之前)拦截 `GET ...?x-image-process=image/resize,w_,h_,m_`,自己解析桶/键 → `Open` → `imageproc.ResizeReader` → 整体写出(**无 Range、无派生缓存**)。gofakes3 的 `Backend` 看不到任意 query 参数,所以必须做成 HTTP 中间件。其余请求原样透传给 gofakes3。

### 4.4 S3 兼容层(`internal/s3gw` + gofakes3,唯一对外接口)
为让华为 OBS SDK 等 S3 客户端直接接入(OBS≈S3),用 **gofakes3**(嵌入式库,白嫖 S3 协议:XML/Range/aws-chunked 解码/multipart)+ 我们实现的 `gofakes3.Backend`。
- 即网关主 server,监听 `ZGS_GW_LISTEN`;`main` 里 `gofakes3.New(s3gw.New(svc,st), WithAutoBucket(true))`,再用 `s3Backend.Wrap(faker.Server())` 套上 S3-compat 中间件栈(copy-source 规范化 + 图片处理)。`Wrap` 是 main 与集成测试共用的唯一中间件装配点,避免漂移。
- 映射:S3 桶+键 → `store` 的 `s3_buckets`(桶注册表)+ `s3_keys`(`bucket/key`→root 索引);对象字节走 `object.Service.Put/Open`。
- **ETag=内容 MD5**:PUT 时 gofakes3 自己用 hashingReader 算(我们不算);GET/HEAD 时我们必须回 `Object.Hash`=`meta.MD5`(故 `Service.Put` 在摄取同一遍里多算一个 MD5 存进 `ObjectMeta.MD5`)。
- `WithAutoBucket(true)`:对不存在的桶 PUT 会自动建桶,兼容「app 假定桶已存在/不显式建桶」。
- **不实现 `MultipartBackend`**:gofakes3 用内存 fallback 攒齐分片再调一次 `PutObject` → 仅适合小文件(大文件 = 第二阶段,见 §10)。
- **不验签名**:gofakes3 不校验 SigV2/V4。写安全靠网络隔离,只能绑内网接口。

## 5. 并发模型

- N 个 HTTP 请求 goroutine + **1 个** worker goroutine。
- 所有元数据/队列变更都走 bbolt 事务,**单写者串行化**,无需额外锁。读用 `View`,写用 `Update`。
- 缓存文件用内容寻址路径 + 原子 `rename`;冷读临时文件名带随机后缀避免并发撞名。
- 改动涉及并发路径时**必须** `go test -race`。

## 6. 配置(全部 env,见 `.env.example`)

必填:`ZGS_NODES`(逗号分隔存储节点 RPC)、`ZGS_ETH_RPC`(宿主链 RPC)、`ZGS_PRIVATE_KEY`(出证私钥,hex 无 0x)。
常用可选:`ZGS_GW_LISTEN`(:8080,即对外 S3 端点;不验签名,只能绑内网)、`ZGS_GW_DATA_DIR`(./data)、`ZGS_GW_MAX_SIZE`(默认 4GiB)、`ZGS_GW_BATCH_MAX`(20)、`ZGS_GW_FLUSH_INTERVAL_MS`(3000,必须>0)、`ZGS_GW_MAX_RETRIES`(5)、`ZGS_EXPECTED_REPLICA`(默认=节点数)。

## 7. 构建 / 测试 / 运行

```
make build   # → bin/gateway
make test    # go test ./...
make lint    # gofmt -l . && go vet ./...
make e2e     # ZGS_E2E=1 真网端到端，需 ZGS_PRIVATE_KEY
```
单测用 fake(`fakeDL` / `fakeChain`)隔离 0G,毫秒级。**提交前**至少 `go test -race ./...` + `gofmt -l`(应为空)+ `go vet`。

**真网 e2e(`ZGS_E2E=1`,需 `ZGS_PRIVATE_KEY` 在宿主链测试网有 gas)**:
- `TestLiveE2ESDK`(`e2e_sdk_test.go`)——**推荐**:真实华为 OBS **Java** SDK 经 HTTP 打网关 → 真 0G。`putObject` → worker 提交 Flow tx + 上传分片 → finalized → 删本地缓存 → `getObject` 触发**带 proof 的 0G 冷读**并校验字节。需 JDK + bundle jar(同 `obssdkjava_test.go`,缺则 skip)。
- `TestLiveE2E`(`e2e_test.go`)——较底层:直接调内部 `object.Service`/`uploader`(不走 SDK/HTTP),用于单独验证 chain 后端。
- 跑法:`set -a; . ./.env; set +a; ZGS_E2E=1 go test ./integration/ -run TestLiveE2ESDK -v -timeout 12m`(key 放 gitignored `.env`)。

## 8. 编码约定(改代码时遵守)

- 风格贴合现有代码:注释解释「**为什么**」(不变量、崩溃语义、SDK 行为),而非复述代码。
- 每个状态转换 = 一个 bbolt 事务;新增队列/状态语义时同步改 `requeue`。
- 错误用 `fmt.Errorf("...: %w", err)` 包装;sentinel error 用 `errors.Is` 判断。
- 不要用 SDK 的 `Must*` 构造器(会 `os.Exit` 绕过错误返回);用返回 error 的版本,见 `chain.New`。
- 加了 `ObjectMeta` 字段记得它是 JSON、需向后兼容。
- 动了 `service`/`worker`/`store` 必加/改对应 `_test.go`,并跑 `-race`。

## 9. 维护者必读的坑

1. **内容去重 ⇒ root 多对一**:内容相同 → 同一个 root。多个 S3 桶/键可指向同一 root;**删除只发生在 S3 索引层**(`S3DeleteObjectKey` 只删 `bucket/key→root` 映射,不碰对象本身,也不影响共享同 root 的其它键)。对象一旦摄取,其元数据/缓存/上链永不删除——0G 内容寻址、不可变,数据**不可物理擦除**;有合规擦除需求需单独设计。无「root 级删除」、无「逻辑删除」标志。
2. **`requeue` 是队列一致性的唯一出口**:清空两条队列后按 status 重新入队。绕过它直接写队列会破坏不变量。
3. **SkipTx 的 txHash**:一批全是 SkipTx 时 SDK 不发新 tx、返回零 hash;`BatchUpload` 此时返回空串,worker 对 SkipTx 项也传空 txHash,**避免用零 hash 覆盖真实 hash**。
4. **`FileStatus` 是 any-node 语义**:任一节点报 finalized 即 finalized。曾改成 quorum(需 `ExpectedReplica` 个节点),但因 demo 下单节点滞后会卡死最终化而**回退**。若要改回 quorum,注意默认 `replica=节点数` 会让一个挂掉的节点永久阻塞。
5. **缓存校验只比大小**:热读只校验文件大小(4GB 对象每次重算 hash 不现实);等长位翻转查不出。冷读从 0G 下载是带 merkle proof 的,完整校验在那一层。
6. **上传是异步的**:`Put` 返回即 `pending` 且可读(本地缓存);真正上链由 worker 异步完成。别假设返回后已 finalized。

## 10. 已知限制 / 待办(均为有意为之,非 bug)

- **唯一对外接口是 S3(`internal/s3gw` + gofakes3)= demo 级**:
  - **不验签名**:gofakes3 不校验 SigV2/V4。AK/SK 任意非空值即可;写安全靠网络隔离,**只能绑内网接口**。真鉴权(presigned 验签 / AK/SK 最小权限)= 第二阶段(评估迁 versitygw,见提交历史讨论)。
  - **无 multipart 后端**:gofakes3 用内存 fallback 攒齐分片再调一次 `PutObject` → 仅适合小文件;无分段上传协议,单对象 ≤ `MaxSize`(默认 4GiB)。
  - 无 versioning;**不支持空对象**(0G 无法寻址 0 字节,返回 InvalidArgument);ListObjects 支持 prefix 但**无分页**。
  - conditional PUT(If-Match/If-None-Match)**已支持**,但条件检查与 key 写入**非原子**(检查在慢速摄取之前,写入在之后):并发同 key 的条件 PUT 可能都通过。受架构所限(摄取是流式落盘+算 root,无法塞进一个 bbolt 事务),demo 单写者场景接受此非原子性,不加锁。CopyObject 原生**零拷贝**(只改 `bucket/key→root` 映射,不重传)。
  - **S3 元数据按内容、非按 key**:`s3_keys` 只存 `bucket/key→root`,对象的 ContentType/Filename 取自 `root` 的 `ObjectMeta`(按内容)。故**字节完全相同、但 Content-Type 不同**的两个 key 去重到同一 root 后,GET/HEAD 会返回首个创建者的 Content-Type。demo 影响极小(同字节通常同类型);如需按 key 元数据,需把 key 记录从「裸 root」改成 `{root, contentType, ...}`(store + s3gw 改动,第二阶段)。
  - **OBS SDK 对接**:**Java**(bundle 3.21.11 与 3.25.5;`AuthTypeEnum.V2` + `setPathStyle(true)` + `setAuthTypeNegotiation(false)`)与 **Node.js**(`signature=v2` + `path_style=true` + 整 URL `server`)均已用真实华为 OBS SDK 跑通,put/get/head/list/range/copy/presigned-GET/delete 全过。测试:`integration/obssdk_test.go`(Node,npm 装在 testdata 下)、`integration/obssdkjava_test.go`(Java,javac/java 在 PATH + bundle jar 在 `testdata/obs-java/lib/` 才跑,否则 skip;jar 与 `.class` 已 gitignore)。
  - **CopyObject 经 OBS SDK 已修复**:OBS SDK 把 `X-Amz-Copy-Source` 整体百分号编码(连 `/` 也编码),raw gofakes3 在 url-decode 前按 `/` 切会 panic(`gofakes3.go:734` 的 `parts[1]`)。已由 `s3gw.FixCopySourceHandler` 中间件(`Wrap` 套在 gofakes3 前)规范化该头修复;`copysource_test.go` 覆盖,两个 SDK 实测复制通过。`Backend.CopyObject` 本身是零拷贝(只改 `bucket/key→root` 映射)。
  - **XML 响应 Content-Type 已修复(为 OBS Java SDK)**:gofakes3 不设 XML 响应的 Content-Type,net/http 嗅探成 `text/xml; charset=utf-8`,而 OBS Java SDK 默认严格校验(`setVerifyResponseContentType(true)`)会拒收。`s3gw.FixXMLContentTypeHandler` 中间件在写 body 前把它设为 `application/xml`(对象读 GET/HEAD **不**包裹,保留大对象 sendfile 快路径);故 Java SDK **无需**改 `setVerifyResponseContentType`。`xmlcontenttype_test.go` 覆盖。
- **图片处理 = 仅 `image/resize`**(华为 `?x-image-process=image/resize,w_,h_,m_` 语法,`s3gw.ImageProcessHandler` + `internal/imageproc`),刻意不做华为云完整图片处理;**派生图不缓存**(每次实时算),大图(>20MB)拒绝,非图片对象返回 400,不识别的 spec 透传原图。视频截图(MPC)未做。

## 11. 文档地图

- `README.md` —— 人读的简洁架构 + 数据/控制流。
- `0g-gateway-design.md` —— 原始设计文档(形态 A:OBS→0G 适配层;SDK 真实接口、机密性、Pruner 风险等深入背景)。
- `docs/migration-from-obs.md` —— 华为 OBS SDK 对接文档(连接配置、实测支持矩阵、图片处理、注意事项)。
- `docs/接口说明.md` —— 给不懂 0G 的对接方的一页纸 S3 能力/差异说明。
- `.env.example` —— 配置项清单。
