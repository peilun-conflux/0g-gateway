# zgs-gateway

OBS 形态的对象网关，后端存储为 0G storage（自部署节点 + 任意 EVM 链上的 Flow 合约）。
对应设计文档：`0g-gateway-design.md`（形态 A：OBS → 0G 存储适配层）。

## 核心行为

- `PUT /objects`：收文件 → 本地秒算 merkle root **立即返回**（不等上链）；
  后台 worker 攒批用 `BatchUpload` 一笔链上 tx 提交多个文件（消 nonce 竞争、摊薄 gas）。
- `GET /objects/{root}`：缓存命中直接回；冷数据回源 0G（**带 merkle proof 校验**）后再回，
  支持 Range / HEAD。
- 同内容（SHA256）天然去重：重复 PUT 返回已有对象，不重复上链。
- 任务队列落库（bbolt）：崩溃重启后自动续传；批失败逐 root 对账
  （已上链的用 `SkipTx` 只补分片，没上链的重发 tx），重试超限标 `failed`。
- `pruned` 是告警态：归档部署必须做容量规划（见设计文档"Pruner 风险"）。

## API

| 方法 | 路径 | 说明 |
|---|---|---|
| PUT/POST | `/objects` | multipart 字段 `file`，或裸 body（`X-Filename` + `Content-Type`）→ `{root,status,sha256,size,dedup}` |
| GET | `/objects/{root}` | 对象字节；鉴权开启时需 `?e=<unix过期>&t=<HMAC token>` |
| HEAD | `/objects/{root}` | 仅响应头 |
| GET | `/objects/{root}/status` | `pending→submitted→onchain→finalized / failed`（后端轮询用，不鉴权） |
| DELETE | `/objects/{root}` | 逻辑删除（`X-Admin-Token`）。注意：已上 0G 的数据不可物理删除 |

下载 token 由后端用共享密钥生成：`t = hex(HMAC-SHA256(secret, root + "|" + expUnix))`
（见 `internal/auth`，与 OBS 签名 URL 等价的能力）。

## 运行

```bash
cp .env.example .env   # 填 ZGS_NODES / ZGS_ETH_RPC / ZGS_PRIVATE_KEY
set -a; source .env; set +a
go run ./cmd/gateway
```

## 测试

```bash
make test        # 单元 + 组件级集成（fake 链后端，无网络依赖）
make e2e         # 真网端到端：需 ZGS_PRIVATE_KEY，走真实节点+链
                 # PUT→上链→finalized→清缓存→0G 冷读（带 proof）→字节比对
```

## 设计备忘

- **SDK 版本钉死 `v1.4.3-testnet`**：`@latest` 会解析到 v1.3.0，缺
  `DownloadToWriter` 且 `SplitableUpload` 签名不同。升级前先核对设计文档"真实 SDK 接口"一节。
- **加密（未实现，预留缝）**：若将来启用网关侧加密，改动点只有两处——
  `object.Service.Put` 落缓存前加一次变换（缓存存密文、root 对密文算）、
  `Open`/server 回流前加一次解密。协议必须是"包装一次、密文为准"
  （SDK 加密头含随机 nonce，重复包装 root 会漂移），细节见设计文档"机密性"一节。
  元数据是 JSON 存 bbolt，加字段向后兼容。
- **单对象上限默认 4GiB**（= SDK fragment 大小），保证一对象一 root；
  更大文件需要 manifest 设计，刻意不做。
- 机密性模型：生产将节点 RPC 封进内网（仅网关可达），对齐 OBS 私有桶语义。
