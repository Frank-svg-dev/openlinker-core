# Runtime 压测工具

[English](./README.md)

`runtime-loadtest` 用两种传输方式压测同一套可靠 Runtime 状态机：

- WebSocket 是首选传输。客户端完成身份认证和连接升级，发送 `runtime.hello`，等待
  `runtime.ready`，然后接收服务端主动推送的任务和取消命令。
- HTTP Pull 是长轮询兜底。它创建同样的持久化 Session，并复用同一套 offer、Attempt、
  lease、fence、ACK、取消和恢复标识。

这个命令使用 `go.mod` 锁定版本的公开 Go SDK。报告中的 contract ID、digest、协议版本和
必需功能均来自该 SDK。它不会偷偷走兼容路径：缺少 Node 凭据、契约不一致，或者指定的
传输方式不可用时，压测会直接失败。

## 运行前准备

账号 Auth API、Core API 和专用 Runtime listener 可以使用不同地址。Auth API 创建一次性
测试用户；Core 创建 Agent、Agent Token 和 Run；Runtime 流量必须经过 mTLS listener。
如果这些服务由同一个进程提供，可以不设置 `OPENLINKER_AUTH_API_ROOT`，它会默认使用
`OPENLINKER_API_ROOT`。

先签发一个压测用 Node，其 capacity 必须覆盖所有 Worker Session。客户端 CA 私钥只能
留在签发机器上：

```bash
DATABASE_URL='postgres://...' ./bin/api runtime-node issue \
  --ca-cert /secure/runtime-client-ca.crt \
  --ca-key /secure/runtime-client-ca.key \
  --display-name 'Runtime load generator' \
  --capacity 100 \
  --cert-out ./node-pki/loadtest.crt \
  --key-out ./node-pki/loadtest.key
```

保存 JSON 输出中的 `node_id`。下面的 Runtime 服务端 CA 是给 Core mTLS listener 证书
签名的 CA，不是给 Node 客户端证书签名的客户端 CA。

```bash
export OPENLINKER_NODE_ID='00000000-0000-4000-8000-000000000001'
export OPENLINKER_API_ROOT='http://127.0.0.1:8080/api/v1'
# Cloud 负责注册账号、Core 使用另一个地址时才需要单独设置。
export OPENLINKER_AUTH_API_ROOT='http://127.0.0.1:8080/api/v1'
export OPENLINKER_RUNTIME_URL='https://127.0.0.1:8443'
export OPENLINKER_RUNTIME_MTLS_CERT_FILE="$PWD/node-pki/loadtest.crt"
export OPENLINKER_RUNTIME_MTLS_KEY_FILE="$PWD/node-pki/loadtest.key"
export OPENLINKER_RUNTIME_MTLS_CA_FILE="$PWD/node-pki/runtime-server-ca.crt"
```

私钥、证书、CA、Node ID 和 HTTPS Runtime 地址都是必填项。工具会先检查这些配置，
确认无误后才创建一次性账号或 Agent。每个 Worker 还会把 Attempt 标识、Event 序号、
待确认的 Result ID 和 ACK 状态同步写入 `-state-dir`（目录权限 `0700`、文件权限
`0600`），然后才继续协议流程。Agent Token 和调用权限不会写入这个目录。

## 基础传输测试

默认的 `auto` 模式先连接 WebSocket。传输失败后改用 Runtime Pull，并定期探测
WebSocket 是否恢复；恢复后，它会先接续正在处理的 Attempt，再切回 WebSocket。

```bash
go run ./cmd/runtime-loadtest \
  -api http://127.0.0.1:8080/api/v1 \
  -auth-api http://127.0.0.1:8080/api/v1 \
  -runtime-url https://127.0.0.1:8443 \
  -transport auto \
  -agents 10 -workers-per-agent 1 -node-capacity 10 \
  -runs 100
```

如果测试期间不允许自动切换传输方式，请指定场景：

```bash
# 只用 WebSocket
go run ./cmd/runtime-loadtest \
  -transport ws -scenarios ws-only

# 只用 HTTP 长轮询
go run ./cmd/runtime-loadtest \
  -transport pull -scenarios pull-only
```

前面设置的环境变量会提供 API 地址、Runtime 地址、Node ID 和三个 mTLS 文件。完整参数
请运行 `go run ./cmd/runtime-loadtest -help` 查看。

## 恢复和安全场景

建议分别运行以下场景，这样每项测试的前提和结果更容易判断。

```bash
# WebSocket → Pull → WebSocket，并恢复正在处理的 Attempt
go run ./cmd/runtime-loadtest \
  -transport auto -scenarios ws-pull-ws \
  -switch-after 3s -switch-back-after 8s -result-delay 12s

# 从 Core A 切到 Core B 后继续处理；两边必须共用数据库和契约
go run ./cmd/runtime-loadtest \
  -transport ws -scenarios core-a-b-resume \
  -runtime-url-secondary https://core-b.example.test:8443 \
  -switch-after 3s -result-delay 8s

# Core 已提交 Pull 请求，但客户端故意丢失每类 ACK 的一次响应
go run ./cmd/runtime-loadtest \
  -transport pull -scenarios ack-response-loss \
  -drop-ack-responses assignment,event,result,cancel

# 重复下发不能启动第二次执行；过期 fence 必须被拒绝
go run ./cmd/runtime-loadtest \
  -transport auto -scenarios duplicate-assignment,stale-fence \
  -duplicate-assignments 2 -stale-fence-probes 1
```

1000 路取消场景会启动真实客户端负载，不是进程内的简单竞态测试。每个取消请求都必须
对应一个正在执行的 Worker Session。下面的例子使用 100 个 Agent，每个 Agent 使用
10 个 Agent Token。取消请求会在 Result 截止时间附近发出，让 Core 中的
`Cancel/Result` 和 `Cancel/ACK` 真正发生竞争。

```bash
go run ./cmd/runtime-loadtest \
  -transport auto -scenarios cancel-race \
  -agents 100 -workers-per-agent 10 -node-capacity 1000 \
  -runs 1000 -run-concurrency 250 \
  -result-delay 10s -cancel-delay 10s \
  -cancel-count 1000 -cancel-concurrency 250 \
  -timeout 10m
```

测试 Redis 信号依赖中断时，需要在压测进程之外停止或隔离 Runtime 使用的 Redis，
同时确保选定的 Core 地址还能直接访问。工具会先等 `/readyz` 返回
`signal_dependency_unavailable`，在此之前不会开始计入结果的 Run。随后它会通过
数据库轮询兜底完成 Pull 任务。

```bash
go run ./cmd/runtime-loadtest \
  -transport pull -scenarios redis-signal-outage \
  -redis-outage-observe 60s -runs 100
```

## 报告内容

JSON 报告把 `runtime.transports.ws`、`runtime.transports.pull` 和两者汇总分开记录。
每部分包括：

- hello/ready 建连延迟；
- offer 到 confirm，以及任务下发延迟；
- lease 续期、Event ACK、Result ACK 和取消延迟；
- 重放的 Event/Result ACK，以及丢失响应后恢复的任务 ACK；
- 稳定的错误码计数。

`runtime.switches` 记录地址和传输方式切换；`runtime.resume` 记录恢复决定及耗时。安全
结果必须显示 `duplicate_execution: 0` 和 `stale_fence_accepts: 0`。Redis 场景还必须
显示 `redis_signal_outage_observed: true`，并且
`db_polling_fallback_completions` 大于零。

报告不会包含 Agent Token、私钥内容或证书内容。

## 慢速连接容量测试

`-connection-capacity` 会保留已经建立的每条 mTLS WebSocket，再分批增加连接。它会持续
检查 Core 的健康和就绪状态，并要求至少 99% 的目标 Worker 保持连接。默认配置每批增加
25 个 Worker，每秒建立 2 条连接，每批观察 30 秒，同时运行少量真实任务，最后对最高
候选容量持续确认 5 分钟。

Agent 数量要足够，保证每个 Agent 不超过 10 个 Token。总超时时间必须覆盖完整的爬升和
保持阶段；参数检查会打印当前目标所需的最短时间。

```bash
go run ./cmd/runtime-loadtest \
  -api http://127.0.0.1:8080/api/v1 \
  -auth-api https://cloud.example.test/api/v1 \
  -runtime-url https://127.0.0.1:8443 \
  -transport ws -scenarios ws-only \
  -connection-capacity \
  -agents 60 -workers-per-agent 10 -node-capacity 600 \
  -connection-step-size 25 -connection-step-hold 30s \
  -connect-stagger 500ms \
  -runs 1 -run-concurrency 1 \
  -hold-after-completion 5m -timeout 30m
```

JSON 中的 `connection_capacity_report` 会记录每个成功或失败的阶段、首个失败目标、
通过 5 分钟确认的稳定值，以及预留 20% 运行余量后的建议值。如果一直达到配置目标而
没有出现失败，这个数字只代表容量下限，不能证明主机无法接受更多连接。
