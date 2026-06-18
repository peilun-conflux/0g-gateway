# zgs-gateway

一个 **S3 兼容的对象存储网关**:对外说 S3 协议(华为 OBS SDK 等 S3 客户端可直连),数据落到
**0G 去中心化存储**(自部署存储节点 + 任意 EVM 链上的 Flow 合约)。调用方不用懂 0G,把它当
一个 OBS/S3 端点用即可。纯 Go、单进程、内嵌 bbolt 存元数据,无外部数据库。详尽设计见
`0g-gateway-design.md`。

## 它解决什么

把「上传文件 → 异步上链 → 可下载」封装成标准 S3 接口,并处理好其中的麻烦事:
内容去重、本地缓存、断点续传、崩溃恢复、批量上链省 gas、失败重试与最终性轮询。

## 寻址方式

对外是 **S3 桶 + 对象 key**(`bucket` + `user/123/a.png`)。网关内部是**内容寻址**:
对象的真实 key 是其内容的 merkle root(`0x…`),天然去重 + 可校验。桶/键 → root 的映射由
网关维护,对客户端透明。

> S3 端点**不校验签名**(demo 形态),写安全依赖网络隔离 —— **只能绑内网接口**(推荐
> `127.0.0.1`,勿绑 `0.0.0.0`/公网)。

## 架构总览

```
                 ┌────────────────────── gateway 进程 ──────────────────────┐
   S3 / OBS SDK  │                                                          │
 ──PUT/GET/DEL─► │  s3gw (gofakes3) ──► object service ──┬──► bbolt (元数据+队列+索引) │
   (bucket/key)  │  (S3 协议 + 图片处理)   (摄取/读取管线) └──► 本地缓存 (磁盘文件)     │
                 │                                                          │
                 │  uploader worker ──(Chain 接口)──► chain backend ─────────┼──► 0G 存储网络
                 │  (后台单 goroutine，攒批/对账/重试)                        │   (链上 tx + 存储节点)
                 └──────────────────────────────────────────────────────────┘
```

- **s3gw** — S3 协议层(gofakes3 解码 XML/Range/aws-chunked/multipart),把桶+键操作映射到对象服务;附带华为风格图片处理(`?x-image-process=image/resize,...`)
- **object service** — 上传管线(算哈希/去重/落缓存)、下载(命中缓存或回源 0G)
- **store (bbolt)** — 对象元数据 + 两条任务队列 + 桶注册表 + 桶/键→root 索引,**每次状态变更一个事务**
- **uploader worker** — 独立后台 goroutine,把队列里的对象批量提交 0G、对账、重试
- **chain backend** — 封装 0G SDK,负责真正的上传 / 状态查询 / 带证明的下载

## 数据流

**上传(写完即可读,上链在后台)**
```
PUT /bucket/foo
  ├─ 流式落临时文件，同时算 SHA256 + MD5(S3 ETag)
  ├─ 按 SHA 去重：见过的内容直接复用，不重复上链
  ├─ 算 merkle root → 原子 rename 进本地缓存
  ├─ 写元数据(pending) + 入「待上传队列」 + 记 bucket/key→root
  └─► 200                                     ← 此刻已可下载（读本地缓存）

     …稍后，后台 worker…
  攒批 → 一笔链上 tx 提交整批 → onchain → 轮询最终性 → finalized
```

**下载(本地优先,缺失回源)**
```
GET /bucket/foo ─► 查 bucket/key→root ─► 本地缓存命中？
                                          ├─ 是：校验大小后直接返回（支持 Range / HEAD）
                                          └─ 否：从 0G 下载(带 merkle proof) → 写回缓存 → 返回
```

## 控制流:对象生命周期

```
pending ──► submitted ──► onchain ──► finalized   （成功终态）
（已缓存，  （已交链后端  （tx 已打包，  （存储节点确认）
  待上传）    待回执）      待最终性）
   └──────────────────────────────► failed        （重试耗尽 / 被剪除，终态）
```
- **崩溃自愈**:任务队列落 bbolt;worker 重启后先对账「submitted/失败」的对象(已上链的用
  `SkipTx` 只补分片,没上链的重发 tx)再决定是否重传,不会盲目重复上链。
- **删除**:从桶/键移除映射;0G 上的数据内容寻址、不可物理擦除。
- `pruned`(被存储节点剪除)是告警态,归档部署需做容量规划。

## 支持的 S3 能力

| 类别 | 支持 |
|---|---|
| 桶 | CreateBucket / ListBuckets / HeadBucket / DeleteBucket(空桶);PUT 到不存在的桶会自动建桶 |
| 对象 | PutObject(含条件写 If-Match/If-None-Match)、GetObject(Range)、HeadObject、DeleteObject(s)、CopyObject(零拷贝)、ListObjects(支持 prefix) |
| ETag | = 内容 MD5 |
| 图片处理 | `GET /{bucket}/{key}?x-image-process=image/resize,w_,h_,m_`(仅 resize;m=lfit/fill/fixed) |

华为 OBS SDK 连接配置 + 完整 S3 能力/差异见 `docs/migration-from-obs.md`。

## 运行

```bash
cp .env.example .env          # 填 ZGS_NODES / ZGS_ETH_RPC / ZGS_PRIVATE_KEY
make build && ./bin/gateway   # 默认监听 :8080，对外即 S3 端点

# 用任意 S3 客户端（这里以 aws-cli 为例，path-style）
aws --endpoint-url http://localhost:8080 s3 cp ./hello.txt s3://demo/hello.txt
aws --endpoint-url http://localhost:8080 s3 cp s3://demo/hello.txt ./out.txt
```

## 测试

```bash
make test     # 单元 + 组件级集成（fake 链后端，无网络依赖，秒级；含真实华为 OBS SDK 跑通）
make lint     # gofmt -l . && go vet ./...
make e2e      # 真网端到端：需 ZGS_PRIVATE_KEY；真实 OBS SDK 经 HTTP→上链→finalized→清缓存→0G 冷读比对
```

## 代码结构

```
cmd/gateway       进程入口、配置装配、优雅退出
internal/s3gw     S3 协议层（gofakes3 后端）+ 中间件（copy-source 规范化 / 图片处理 / XML content-type）
internal/object   上传/下载管线
internal/store    bbolt 元数据、任务队列、桶+键→root 索引
internal/uploader 后台批量上传 worker
internal/chain    0G SDK 封装
internal/imageproc 图片缩放（纯标准库 image/resize）
integration       真网端到端测试（ZGS_E2E=1 才跑）+ 华为 OBS SDK 兼容测试
```

## 设计备忘

- **SDK 版本钉死 `v1.4.3-testnet`**:`@latest` 会解析到 v1.3.0,接口不兼容。升级前先核对设计文档。
- **不用 SDK 的 `Must*` 构造器**:它们出错即 `os.Exit`,会绕过错误返回;统一用返回 error 的版本。
- **单对象上限默认 4 GiB**(= SDK fragment 大小),保证一对象一 root;更大文件需 manifest 设计,刻意不做。无 multipart 后端(gofakes3 内存攒分片,仅小文件)。
- **加密(未实现,预留缝)**:若启用网关侧加密,改动点只有两处——`Put` 落缓存前加变换(缓存存密文、
  root 对密文算)、`Open` 回流前解密;协议须"包装一次、密文为准"。元数据 JSON 存 bbolt,加字段向后兼容。
- **机密性**:生产将节点 RPC 封进内网(仅网关可达),对齐 OBS 私有桶语义。

> 维护代码请读 `CLAUDE.md`(架构、不变量、坑);对接方一页纸能力说明见 `docs/接口说明.md`;更多文档见 `docs/`。
