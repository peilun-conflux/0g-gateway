# zgs-gateway

**S3 兼容的对象存储网关**:对外说 S3 协议(华为 OBS SDK 等 S3 客户端直连),数据落到
**0G 去中心化存储**(自部署存储节点 + EVM 链上的 Flow 合约)。调用方把它当一个 OBS/S3 端点用即可,
不用懂 0G。纯 Go、单进程、内嵌 bbolt,无外部依赖。

> ⚠️ S3 端点**不校验签名**(demo 形态),写安全依赖网络隔离 —— **只能绑内网接口**
> (推荐 `127.0.0.1`,勿绑 `0.0.0.0` / 公网)。

## 部署运行

前置:**Go 1.24+**;一套可达的 0G 存储节点 + 宿主链(EVM)RPC + 一个在该链有 gas 的出证私钥。

```bash
cp .env.example .env     # 填 ZGS_NODES / ZGS_ETH_RPC / ZGS_PRIVATE_KEY（见下）
make build               # → bin/gateway
./bin/gateway            # 默认监听 :8080，对外即 S3 端点
```

## 配置(全部 env，见 `.env.example`)

**必填**

| 变量 | 说明 |
|---|---|
| `ZGS_NODES` | 0G 存储节点 JSON-RPC,逗号分隔 |
| `ZGS_ETH_RPC` | 宿主链(EVM)RPC,Flow 合约所在链 |
| `ZGS_PRIVATE_KEY` | 出证账户私钥(hex 无 0x);需在宿主链有 gas |

**常用可选**

| 变量 | 默认 | 说明 |
|---|---|---|
| `ZGS_GW_LISTEN` | `:8080` | 监听地址(对外 S3 端点)。⚠️ 绑内网 |
| `ZGS_GW_DATA_DIR` | `./data` | 本地缓存 + bbolt 元数据目录 |
| `ZGS_GW_MAX_SIZE` | `4 GiB` | 单对象上限(无 multipart,须一次 PUT 传完) |
| `ZGS_GW_BATCH_MAX` | `20` | 上链攒批上限 |
| `ZGS_GW_FLUSH_INTERVAL_MS` | `3000` | worker 刷新间隔(ms,须 > 0) |
| `ZGS_GW_MAX_RETRIES` | `5` | 上链重试上限 |
| `ZGS_EXPECTED_REPLICA` | 节点数 | 期望副本数 |

## 使用

对外是标准 **S3 桶 + 对象 key**,任意 S3 客户端用**路径风格(path-style)**直连即可:

```bash
aws --endpoint-url http://localhost:8080 s3 cp ./hello.txt s3://demo/hello.txt
aws --endpoint-url http://localhost:8080 s3 cp s3://demo/hello.txt ./out.txt
```

**华为 OBS SDK**:`signature=v2` + `path_style=true` + endpoint 指向网关(AK/SK 任意,不验签名)。
Java / Node.js 连接配置、支持的 S3 操作、图片处理(`?x-image-process=image/resize,...`)与差异,
见 [`docs/migration-from-obs.md`](docs/migration-from-obs.md);一页纸能力说明见
[`docs/接口说明.md`](docs/接口说明.md)。

接入要点:
- **写完即可读**:PUT 返回即落本地缓存可读;真正上链由后台异步完成(勿假设刚 PUT 就已 finalized)。
- **内容去重**:相同字节只上链一次,迁移可重入(中断重跑不重复花 gas)。
- **冷读**:本地缓存缺失时从 0G 带 merkle proof 拉回。
- **限制**:不支持空对象;无 multipart / versioning;ListObjects 无分页。

## 测试

```bash
make test     # 单元 + 组件级集成（fake 链后端，秒级；含真实华为 OBS SDK 跑通）
make lint     # gofmt -l . && go vet ./...
make e2e      # 真网端到端：需 ZGS_PRIVATE_KEY；真实 OBS SDK 经 HTTP→上链→finalized→0G 冷读比对
```

## 文档

- 架构与代码说明(人读)—— [`docs/architecture.md`](docs/architecture.md)
- 维护手册(给 AI agent:不变量、坑、约定)—— [`CLAUDE.md`](CLAUDE.md)
- OBS / S3 SDK 对接 —— [`docs/migration-from-obs.md`](docs/migration-from-obs.md)、[`docs/接口说明.md`](docs/接口说明.md)
- 原始设计 —— `0g-gateway-design.md`
