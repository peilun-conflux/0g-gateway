# 0G 存储网关设计(形态 A:OBS → 0G 存储适配层)

## 目标
把存证后端里的"华为云 OBS 对象存储"这一层,替换成调用 0G storage 的网关。
前端(ipp.baoquan.com 那套)完全不动;后端只把"写/读 OBS"改成"写/读网关"。

## 核心架构决策

### 1. 内容寻址 vs 路径寻址(必须先想清楚)
- OBS:key 是后端自选的路径(path-addressed),`ossUrl = https://obs.../你选的path`
- 0G:key 是文件内容算出的 **merkle root**(content-addressed),上传后才"得到" key
- 适配方式:网关上传返回 root,后端把 **root 当作对象 key 存库**;
  `ossUrl = https://gateway/objects/{root}`。天然去重(同内容 → 同 root)。

### 2. root 可以"不上传就秒算"——解决前端等 20 秒的关键
`core.MerkleRoot(filepath)` 纯本地计算,不触网、不上链。于是网关 PUT 可以:
1. 收文件 → 落本地缓存
2. **秒算 root,立刻返回给后端**(后端马上能存库、回前端)
3. 后台异步做"上链 submit + 分片上传到 0G"
读请求在 finalize 前由本地缓存兜底,完全解耦慢的链上部分。
⚠️ 若启用网关侧加密,"秒算"的对象是**密文** root(PUT 时一次性包装加密→密文落缓存→对密文算 root,
仍是纯本地操作);`core.MerkleRoot(明文路径)` 的结果在加密模式下不是对象 key。见"工程要点·机密性"。

### 3. 文件哈希 ≠ 0G root(存证场景要分清)
- 存证证书上的"文件指纹":继续用 SHA256 / 国密 SM3(司法认可的算法)
- 0G root:keccak merkle root,是 0G 的寻址 key + 链上存在性证明
- 两个都存。证书里可同时印:SHA256/SM3 + 0G root + txHash + flow 合约地址。
  第三方验真分两步:拿 **root** 对节点 `download --proof`(CLI 只认 root,cmd/download.go:53,
  txHash 不是下载输入);拿 txHash 在链上核对 submit 记录。(比 OBS 强的卖点)
- ⚠️ 该验真能力以**数据未被 prune**为前提:prune 会删 chunks、删 root→txSeq 索引并标记 Pruned
  (node/storage tx_store.rs:201,214)。"永久可验真"要靠容量规划+副本策略保障,不是协议自带的。

## 网关 API 契约(最小集)
- `PUT /objects` (body=文件)         → `{"root":"0x..","status":"pending"}`
- `GET /objects/{root}`              → 文件字节(缓存优先,回源 0G 带证明)
- `GET /objects/{root}/status`       → `pending|submitted|onchain|finalized|failed`
  (节点侧 unavailable/pruned 要映射成告警态;状态命名可参考 SDK 自带 gateway/local_apis.go
   的 unavailable|pruned|available|finalized)
- (可选) `HEAD /objects/{root}`      → 存在性 / 大小
- 鉴权:GET 带过期签名 token(对标 OBS 签名 URL);PUT 仅后端内网可达。见"工程要点·机密性/鉴权"

## OBS → 网关 改造对照
| 后端原操作 | 改成 |
|---|---|
| `obs.PutObject(bucket,key,stream)` | `PUT gateway/objects`,拿返回的 root 当 key 存库 |
| 存库字段 `oss_path` | 改存 `root`(0x 开头 66 字符) |
| `ossUrl = obs域名/key` | `ossUrl = gateway域名/objects/{root}` |
| `obs.GetObject(key)` | `GET gateway/objects/{root}` |

## 状态机(异步上链)
`pending`(本地缓存,root 已知) → `submitted`(tx 已发) → `onchain`(receipt 确认成功) → `finalized`(节点确认);
任一步失败进 `failed`,带重试计数。后端轮询 `/objects/{root}/status`;或网关回调后端 webhook。
- **任务队列必须落库**(root, local_path, tx_hash, status, retry_n),网关重启后扫描未完成任务续传;
  否则崩溃 = 缓存里有数据但永远不上链。
- **PUT 返回前的原子顺序**:缓存文件 fsync → 任务记录+对象元数据落库(同一事务) → 返回 root。
  节点 chunk pool 的内存缓存会被 GC(node/chunk_pool chunk_cache.rs:97),不能当崩溃恢复依据。
- 进度/健康看 `zgs_getFileInfo` 的 `uploadedSegNum`/`finalized`/`pruned`;pruned=true 时 finalized 会变回 false,要告警。
- 实测节点源码(node/rpc/src/zgs/impl.rs:94):segment 已持久化、未 pruned 即可下载,**不要求 finalized**——
  本地缓存实际只需兜底到"segment 传完"为止,窗口比想象的短。

## 工程要点
- **机密性(与 OBS 最大的差异,必须先定方案)**:OBS 是私有 bucket;0G 是内容寻址,
  任何拿到 root 的人都能直接从存储节点 RPC 拉数据。生产二选一:
  (a) 网络层封闭节点 RPC(仅网关可达);
  (b) 网关侧客户端加密(AES-256-CTR)。SDK 加密头每次生成**随机 nonce**(core/encryption.go:36),
      同内容重复包装 root 也不同,因此协议必须是"**包装一次,密文为准**":
      1. 收明文 → 算 SHA256/SM3(证书指纹);网关 DB 按明文哈希查重,命中直接复用已有 root;
      2. `core.NewEncryptedData(file, key)`(core/encrypted_data.go:20)生成一次性 nonce,
         把密文流落盘为缓存文件——只物化这一次;
      3. 对**密文文件**算 `core.MerkleRoot`,返回该 root 作为对象 key(仍是纯本地秒算);
      4. 异步上传/重试永远复用这份密文文件,且**不带** EncryptionKey 选项——密文当普通
         字节上传,避免 SDK 二次包装出新 nonce/新 root;BatchUpload"不处理加密"的坑也随之规避;
      5. 读路径:网关持 key,密文头 17 字节含 nonce,自行 CTR 流式解密回流——不依赖 SDK
         文件级的 WithEncryptionKey,也绕开 writer 接口不支持加密文件的限制。
      证书上 SHA256/SM3(明文指纹)与密文 root 的对应关系要在验真说明里写清。
      另注意:网关 DB 层按明文哈希去重会让"同内容同 root"跨用户可见;司法存证若在意
      该侧信道,可关闭跨用户去重(代价是重复内容多付一份存储+gas)。
- **鉴权**:`GET /objects/{root}` 不能裸奔——OBS 的对等能力是签名 URL,
  网关同样做带过期时间的签名 token,防 root 泄露/遍历。
- **密钥/Gas**:出证账户私钥放网关侧(KMS/vault/env,勿裸存);监控 CFX 余额。
  高并发优先用 `BatchUpload`:攒批(满 N 个文件或 T 秒)后**一笔 tx 提交多个文件**,
  天然消掉 nonce 竞争、gas 摊薄;多账户池只作兜底。实测单笔存证 gas ≈ 0.0039 CFX,极低。
  ⚠️ BatchUpload 两个坑(已核源码 transfer/uploader_batch.go):
  ① **不处理加密**——直接对传入数据建树上传(uploader_batch.go:101),DataOptions 里的
     EncryptionKey 不生效;启用加密时要参照 upload_dir.go:104 的做法,先把每个文件包成
     密文 IterableData 再传入,否则**明文上 0G**。
  ② 部分失败时返回 `(txHash, nil, err)`,**不返回 roots**(uploader_batch.go:217)——
     网关必须在调用前自算每个文件(密文)root 并落库;失败后按 root 逐个查 zgs_getFileInfo
     对账,决定哪些单文件重传,而不是整批重来重复花 gas。
     重传两个约束:`SkipTx` 仅当节点已能按 root 查到链上 entry 时可用(uploader.go:373,453),
     查不到的必须重新走带 tx 的上传;加密模式下必须复用已落盘的同一份密文文件(见机密性),
     重新包装会出新 root,对不上账。
- **元数据**:0G 只存原始字节。content-type、原始文件名、大小、SHA256/SM3 随 root 存库,
  GET 时回填 Content-Type / Content-Disposition;Range 请求(视频回放)直接用
  `DownloadRangeToWriter`(SDK 原生支持,注意无 proof)。
- **持久性**:本地缓存是写穿缓冲,不是唯一副本。生产 ≥3 节点 + 监控 finalized;
  缓存设 LRU/TTL,冷数据回源 0G。
- **Pruner 风险(归档场景必须管)**:节点 config-testnet-turbo.toml 配了
  `db_max_num_sectors = 4e9`(约 1TB),DB 到 90% 会触发 shard 扩容并**真删分片外数据**;
  存证是永久归档,要做容量规划+调大该值+告警,并把 FileInfo.pruned 纳入巡检。
- **大文件**:`SplitableUpload` 按 fragmentSize(默认 4GB)自动分片,返回多个 root。
  对象语义下需限制单对象大小,或网关维护 objectId→[]root 清单。
  另:FastMode 实际只对 **≤256KB** 的数据生效(fastUploadMaxSize,超限 SDK 自动退回
  慢路径并打日志,transfer/uploader.go:33,355)。节点侧两个独立上限别混淆:
  tx 未见时单文件可缓存上限 10MB(rpc config.rs:26 max_cache_file_size)、
  chunk pool 总内存缓存约 1GiB(4M chunks,node/src/config/mod.rs:55)。
- **可用性**:你现在 2 节点 numShard=1(全量副本);生产按副本/分片策略扩。
  网关若多实例,本地缓存要共享(NFS/MinIO)或按 root 粘性路由,否则 pending 期读请求会打偏。
- **幂等**:同内容并发 PUT 按 root 加锁 + DB 唯一键;已存在且非 failed 直接返回现状态,
  配合 `SkipTx` 避免重复上链花 gas。空文件(0 字节)直接拒绝(0G 无法寻址)。

## 迁移路径
1. 双写期:后端同时写 OBS + 网关,新数据 ossUrl 走网关
2. 回填:OBS 存量批量灌入 0G,建 oldKey→root 映射
3. 切读:ossUrl 全走网关,OBS 转冷备/下线

## 技术选型
Go + 官方 `github.com/0gfoundation/0g-storage-client` 当库 import。
直连你那两台节点 + Conflux eSpace flow 合约 `0x3ff03285aa79027ecc552432336fcb85ead7199e`。

## 真实 SDK 接口(已核对 **v1.4.3-testnet** 源码,即 main HEAD;本地克隆 /tmp/claude/0g-storage-client)
⚠️ **版本钉死**:GitHub Releases 页最新"正式版"是 v1.3.0(2026-02),`go get @latest` 会解析到它,
但 v1.3.0 **没有** DownloadToWriter,且 `SplitableUpload` 多一个独立 fragmentSize 参数(签名不同)。
本设计按 v1.4.3-testnet 写,go.mod 必须显式 `go get github.com/0gfoundation/0g-storage-client@v1.4.3-testnet`。

- 算 root:           `core.MerkleRoot(path string) (common.Hash, error)`
- web3:              `blockchain.MustNewWeb3(url, key string, opt ...providers.Option) *web3go.Client`
- uploader:          `transfer.NewUploaderFromConfig(ctx, w3, transfer.UploaderConfig{...}) (*Uploader, closer, error)`
- 单文件上传:       `uploader.Upload(ctx, data core.IterableData, opt ...UploadOption) (txHash, root common.Hash, err)`
- 分片上传:         `uploader.SplitableUpload(ctx, data core.IterableData, opt ...UploadOption) (txHashes, roots []common.Hash, err)`
                     (fragmentSize 在 `UploadOption.FragmentSize`,0 = 默认 4GiB;`BatchSize` 默认 10)
- 攒批上传:         `uploader.BatchUpload(ctx, datas []core.IterableData, opt ...BatchUploadOption) (txHash common.Hash, roots []common.Hash, err)`
                     ——多个文件合并成**一笔链上 tx**,高并发首选
- 费用预估:         `uploader.EstimateFee(ctx, data, tags)` / `EstimateBatchFee(ctx, datas, tags)`
- 打开文件:         `core.Open(path) (*core.File, error)`  (实现 IterableData)
- 下载 client:       `node.MustNewZgsClients(urls []string, opt ...providers.Option) []*ZgsClient`
- downloader:        `transfer.NewDownloader(clients []*node.ZgsClient, opts ...zg_common.LogOption) (*Downloader, error)`
- 落文件下载:       `downloader.Download(ctx, root string, filename string, withProof bool) error`
- 流式下载:         `downloader.DownloadToWriter(ctx, root string, w io.Writer, withProof bool) error`
                     (transfer/downloader_writer.go,顺序拉 segment 直写 w,专为 HTTP 网关设计)
- Range 下载:        `downloader.DownloadRangeToWriter(ctx, root string, offset, length int64, w io.Writer) error`
                     (无 proof 校验,热路径用;完整性校验仍走 Download+withProof)
  ⚠️ 两个 writer 接口都**不支持加密文件**(CTR 头需先缓冲解析)——若启用网关侧加密,
  读路径退回 `Download` 落缓存文件再回流,或网关持密钥自行做 CTR 流式解密。
- 加密:             `UploadOption.EncryptionKey []byte`(32字节 AES-256-CTR) /
                     `RecipientPubKey`(ECIES) / `downloader.WithEncryptionKey(key)`
- UploadOption 关键字段:内嵌 TransactionOption(Submitter/Fee/Nonce/MaxGasPrice/NRetries/Step)、
  FinalityRequired(FileFinalized/TransactionPacked)、ExpectedReplica、TaskSize、
  SkipTx(已存在不重复上链)、FastMode(不等 receipt;仅 ≤256KB 生效,超限自动禁用)、
  Method("min")、FragmentSize、BatchSize、EncryptionKey
- 另:SDK 仓库自带 `gateway` 包只是 127.0.0.1:6789 的 CLI 本地辅助 API(upload/download/status),
  非生产网关,不影响自建网关的必要性,但其 status 实现可参考。
