# 0G Storage Gateway — 架构与代码说明

面向开发者的架构 + 代码结构说明(人读)。部署 / 使用 / 配置见 [`../README.md`](../README.md);
给 AI agent 的维护手册(不变量、坑、约定)见 [`../CLAUDE.md`](../CLAUDE.md);原始设计见
`../0g-gateway-design.md`。

## 形态

**S3 兼容端点 → 0G 去中心化存储。** 对外是 **S3 桶 + 对象 key**(`bucket` + `user/123/a.png`),
内部是**内容寻址**:对象的真实 key 是其内容的 merkle root(`0x…`,服务端生成,天然去重 + 可校验),
桶/键 → root 的映射由网关维护、对客户端透明。纯 Go、单进程、内嵌 bbolt 存元数据,无外部数据库。

> 早期还有一套原生 HTTP API(`/objects` + `/kv` + HMAC 签名 URL),因「S3 是唯一接入方式」已整体删除;
> 现在唯一对外接口是 S3(`internal/s3gw`)。

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

- **s3gw** — S3 协议层(gofakes3 解码 XML/Range/aws-chunked/multipart),把桶+键操作映射到对象服务;前置三个中间件:copy-source 头规范化、华为风格图片处理(`?x-image-process=image/resize,...`)、XML 响应 Content-Type 规范化。
- **object service** — 上传管线(算哈希/去重/落缓存)、下载(命中缓存或回源 0G)。
- **store (bbolt)** — 对象元数据 + 两条任务队列 + 桶注册表 + 桶/键→root 索引,**每次状态变更一个事务**。
- **uploader worker** — 独立后台 goroutine,把队列里的对象批量提交 0G、对账、重试。
- **chain backend** — 封装 0G SDK,负责真正的上传 / 状态查询 / 带证明的下载。

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

## 对象生命周期(状态机)

```
pending ──► submitted ──► onchain ──► finalized   （成功终态）
（已缓存，  （已交链后端  （tx 已打包，  （存储节点确认）
  待上传）    待回执）      待最终性）
   └──────────────────────────────► failed        （重试耗尽 / 被剪除，终态）
```
- **崩溃自愈**:任务队列落 bbolt;worker 重启后先对账「submitted/失败」的对象(已上链的用 `SkipTx` 只补分片,没上链的重发 tx)再决定是否重传,不会盲目重复上链。对账时若节点不可达,**跳过本轮、留队列下次重试**,不在状态未知时盲发 tx。
- **删除**:从桶/键移除映射;0G 上的数据内容寻址、不可物理擦除。
- `pruned`(被存储节点剪除)是告警态,归档部署需做容量规划。

## 代码结构

```
cmd/gateway        进程入口、配置装配、优雅退出
internal/s3gw      S3 协议层(gofakes3 后端)+ 中间件(copy-source / 图片 / XML content-type)
internal/object    摄取/读取管线(去重、缓存、冷读回源)
internal/store     bbolt 元数据、上传/最终化队列、桶+键→root 索引
internal/uploader  后台批量上传 worker(攒批/对账/重试/最终性轮询)
internal/chain     0G SDK 封装(上传 / 状态查询 / 带证明下载)
internal/imageproc 图片缩放(纯标准库 image/resize)
integration        真网 e2e + 华为 OBS SDK(Java/Node.js)兼容测试
```

依赖方向:`s3gw → object → store`;`uploader → store`(+ `Chain` 接口);`chain` 实现 `uploader.Chain` 并被 `main` 注入(`uploader` 不依赖 `chain` 包,靠接口解耦便于 fake 测试)。

## 设计备忘

- **SDK 版本钉死 `v1.4.3-testnet`**:`@latest` 会解析到 v1.3.0,接口不兼容。升级前先核对设计文档。
- **不用 SDK 的 `Must*` 构造器**:它们出错即 `os.Exit`,会绕过错误返回;统一用返回 error 的版本。
- **单对象上限默认 4 GiB**(= SDK fragment 大小),保证一对象一 root;更大文件需 manifest 设计,刻意不做。无 multipart 后端(gofakes3 内存攒分片,仅小文件)。
- **加密(未实现,预留缝)**:若启用网关侧加密,改动点只有两处——`Put` 落缓存前加变换(缓存存密文、root 对密文算)、`Open` 回流前解密;协议须"包装一次、密文为准"。元数据 JSON 存 bbolt,加字段向后兼容。
- **机密性**:生产将节点 RPC 封进内网(仅网关可达),对齐 OBS 私有桶语义。S3 端点不验签名,只能绑内网。
