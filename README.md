# sync-over-http（中文文档）

`sync-over-http` 是一个基于 HTTP 的单向镜像同步工具，目标是提供类似 rsync 的镜像能力（上游镜像 -> 本地镜像）。

本项目当前特性：

- 单向拉取同步（pull 模式）
- 多模块（module）隔离
- 模块级锁（server 侧锁）
- WebSocket 解锁通知（客户端等待解锁后立即继续）
- 模块级 token 鉴权
- 排除规则（glob + regex，排除优先）
- 删除传播 + 删除安全阈值保护
- 断点续传（HTTP Range）
- 保留文件 `mode` 与 `mtime`
- 审计日志（JSONL，按天滚动，保留 30 天）

---

## 1. 架构与角色

- `sync-server`
  - 托管上游镜像数据
  - 按模块提供快照清单与对象下载
  - 管理模块锁与 WebSocket 等待
  - 输出审计日志

- `sync-client`
  - 一次性执行同步（one-shot）
  - 常由外部调度器或其他程序触发
  - 在模块被锁时通过 WebSocket 等待服务端通知

---

## 2. 快速开始

### 2.1 准备配置文件

复制并修改 `server.example.yaml`，示例：

```yaml
server:
  root_dir: /data/upstream
  lock_dir: /var/lib/sync-http/locks
  audit_dir: /var/log/sync-http/audit
  audit_retention_days: 30
  page_size: 5000
  lock_ttl: 10m
  snapshot_ttl: 30m

modules:
  - name: core
    root: core
    tokens:
      - core-token
  - name: docs
    root: docs
    tokens:
      - docs-token
```

说明：

- `root_dir`：上游根目录
- `modules[].root`：模块根目录（相对 `root_dir` 或绝对路径）
- 安全约束：模块目录必须位于 `root_dir` 下

### 2.2 启动服务端

```bash
go run ./cmd/sync-server -config ./server.yaml -listen :8080
```

### 2.3 执行一次客户端同步

```bash
go run ./cmd/sync-client \
  -server http://127.0.0.1:8080 \
  -token core-token \
  -module core \
  -source . \
  -target /data/local-mirror \
  -exclude-glob "tmp/**" \
  -exclude-regex ".*\\.bak$"
```

---

## 3. 配置说明（server.yaml）

### 3.1 server 字段

- `server.root_dir`（必填）
- `server.lock_dir`（可选，默认 `.sync-locks`）
- `server.audit_dir`（可选，默认 `./audit`）
- `server.audit_retention_days`（可选，默认 `30`）
- `server.page_size`（可选，默认 `5000`）
- `server.lock_ttl`（可选，默认 `10m`）
- `server.snapshot_ttl`（可选，默认 `30m`）

### 3.2 modules 字段

每个模块配置：

- `name`：模块名（必填，唯一）
- `root`：模块根目录（必填，必须位于 `root_dir` 下）
- `tokens`：该模块可用的 Bearer Token 列表（至少一个）

加载策略：

- 启动时加载（静态配置）
- 修改 YAML 后需要重启服务生效

---

## 4. 客户端参数说明

`cmd/sync-client` 支持：

- `-server`：服务端地址，默认 `http://127.0.0.1:8080`
- `-token`：Bearer token
- `-module`：模块名（必填）
- `-source`：模块内源路径，默认 `.`
- `-target`：本地镜像目录
- `-client-id`：客户端标识
- `-exclude-glob`：排除 glob（可重复）
- `-exclude-regex`：排除 regex（可重复）
- `-delete-guard-ratio`：删除比例阈值，默认 `0.10`
- `-delete-guard-min-files`：最小文件数门槛，默认 `1000`
- `-force-delete-guard`：强制跳过删除保护
- `-dry-run`：仅规划，不写盘
- `-page-size`：清单分页大小，默认 `5000`

---

## 5. 同步流程

1. 客户端请求 `POST /v1/sessions`（必须带 `module`）
2. 若模块被锁，服务端返回 `423 Locked`
3. 客户端连接 `GET /v1/locks/wait/ws?module=<name>`
4. 服务端推送 `unlocked` 事件后，客户端立即重试会话创建
5. 获取快照清单（分页）
6. 计算下载/删除计划
7. 执行删除安全检查
8. 下载变更对象（支持 Range 续传）
9. 校验 checksum，更新 mode/mtime，原子替换到目标目录
10. 执行删除传播
11. 提交 `commit`

---

## 6. API 说明

所有受保护接口都需要请求头：

`Authorization: Bearer <token>`

### 6.1 健康检查

- `GET /healthz`

### 6.2 模块锁

- `POST /v1/locks`
  - 请求体：
    ```json
    {"module":"core","owner":"upstream-syncer","ttl_seconds":600}
    ```

- `POST /v1/locks/release`
  - 请求体：
    ```json
    {"module":"core","owner":"upstream-syncer"}
    ```

### 6.3 锁等待 WebSocket

- `GET /v1/locks/wait/ws?module=core`

事件示例：

- `{"event":"locked","module":"core","owner":"upstream-syncer"}`
- `{"event":"unlocked","module":"core"}`

### 6.4 会话创建

- `POST /v1/sessions`
  - 请求体示例：
    ```json
    {
      "module":"core",
      "source_path":".",
      "exclude_globs":["tmp/**"],
      "exclude_regex":[".*\\.bak$"],
      "client_id":"sync-client",
      "requested_page_size":5000
    }
    ```

说明：

- `module` 必填
- 未带 `module` 的旧请求会被拒绝

### 6.5 清单与对象

- `GET /v1/snapshots/{snapshot_id}/manifest?page_size=5000&cursor=0`
- `GET /v1/objects?snapshot_id={snapshot_id}&path={path}`

### 6.6 会话提交

- `POST /v1/sessions/{session_id}/commit`

---

## 7. 锁与并发语义

- 锁粒度：模块级（每个模块单独锁）
- 公平性：先到先得不保证，采用“谁先重试成功谁获得执行权”
- 多模块互不影响：`moduleA` 被锁不会阻塞 `moduleB`

---

## 8. 安全与权限模型

- Token 为模块级权限
- 请求访问某模块时，token 必须在该模块 `tokens` 列表内
- WebSocket 等待接口同样校验模块 token
- 模块根目录受 `root_dir` 边界保护，防止越界访问

### 8.1 Token 配置示例

`server.yaml` 中每个模块单独配置可用 token：

```yaml
modules:
  - name: core
    root: core
    tokens:
      - core-token-prod-001
      - core-token-ops-001
  - name: docs
    root: docs
    tokens:
      - docs-token-prod-001
```

要点：

- token 只在所属模块生效，不能跨模块访问
- 可以给同一模块配置多个 token，便于轮换与分角色使用
- 建议按环境区分（prod/staging），不要复用同一 token

### 8.2 请求中携带 Token

所有受保护 API 都用 Bearer 头：

```http
Authorization: Bearer <token>
```

HTTP 示例：

```bash
curl -X POST http://127.0.0.1:8080/v1/sessions \
  -H 'Authorization: Bearer core-token-prod-001' \
  -H 'Content-Type: application/json' \
  -d '{"module":"core","source_path":".","client_id":"cli"}'
```

WebSocket 示例（必须也带 Authorization 头）：

```bash
curl --http1.1 -i -N \
  -H 'Connection: Upgrade' \
  -H 'Upgrade: websocket' \
  -H 'Sec-WebSocket-Version: 13' \
  -H 'Sec-WebSocket-Key: SGVsbG9Xb3JsZDEyMw==' \
  -H 'Authorization: Bearer core-token-prod-001' \
  'http://127.0.0.1:8080/v1/locks/wait/ws?module=core'
```

### 8.3 鉴权失败示例

常见失败：

- 缺少 Authorization 头 -> `401 Unauthorized`
- token 不在该模块白名单 -> `401 Unauthorized`
- module 不存在 -> `401 Unauthorized`（当前实现）

排查建议：

- 先确认 `module` 名字拼写
- 再确认 token 是否出现在对应 `modules[].tokens`
- 最后确认请求头是否是 `Authorization: Bearer ...`

### 8.4 Token 轮换建议（不停机）

由于服务端配置是启动加载，推荐轮换流程：

1. 在 YAML 中为模块新增新 token（保留旧 token）
2. 重启 `sync-server`
3. 客户端切换到新 token
4. 验证无误后从 YAML 删除旧 token
5. 再次重启 `sync-server`

### 8.5 生产安全建议

- 不要把 token 硬编码到代码仓库
- 通过环境变量或密钥管理系统注入（如 systemd EnvironmentFile）
- 对 token 做最小权限划分（按模块/按用途拆分）
- 开启 HTTPS（反向代理）避免明文传输
- 定期轮换 token 并结合审计日志追踪调用方

注意：当前示例使用明文 HTTP。生产环境建议：

- 通过反向代理启用 HTTPS
- 仅内网可达
- 配置访问控制与审计采集

---

## 9. 删除安全保护

客户端会在执行删除前检查：

- 本地文件总数 >= `delete-guard-min-files`
- `planned_deletes / total_local > delete-guard-ratio` 时中止

可通过 `-force-delete-guard` 强制绕过。

---

## 10. 审计日志

服务端审计输出到 JSONL：

- 目录：`server.audit_dir`
- 文件：`audit-YYYY-MM-DD.jsonl`
- 保留：默认 30 天（启动时清理过期文件）

典型事件：

- `lock_acquired`
- `lock_released`
- `session_created`
- `session_locked`
- `session_committed`
- `ws_wait_start`
- `ws_wait_unlocked`

---

## 11. 锁文件机制（给 `apt-sync.py` 使用）

你提到要和 `apt-sync.py` 配合，这里给出可直接落地的锁使用规范。

### 11.1 先说结论

- `apt-sync.py` **不要直接写本地 lock 文件**。
- 正确做法是：通过 `sync-server` 的锁 API 申请/释放锁。
- `sync-server` 会在 `server.lock_dir` 下维护 lock 文件，作为服务端锁状态落盘。

### 11.2 锁作用

- 锁粒度是 **模块级**。
- 一个模块在上游同步中（`apt-sync.py` 正在跑）时，客户端拉取会话会收到 `423 Locked`。
- 客户端随后通过 WebSocket 等待解锁事件，避免盲目轮询。

### 11.3 锁生命周期（推荐）

1. `apt-sync.py` 启动前先调用 `POST /v1/locks` 抢锁。
2. 抢锁成功后开始执行真正的上游同步。
3. 无论成功失败，都在 `finally` 调用 `POST /v1/locks/release` 释放锁。
4. 若进程异常退出未释放，锁会在 `ttl_seconds` 到期后自动失效。

### 11.4 API 示例（模块 `core`）

加锁：

```bash
curl -X POST http://127.0.0.1:8080/v1/locks \
  -H 'Authorization: Bearer core-token' \
  -H 'Content-Type: application/json' \
  -d '{"module":"core","owner":"apt-sync.py@hostA","ttl_seconds":3600}'
```

解锁：

```bash
curl -X POST http://127.0.0.1:8080/v1/locks/release \
  -H 'Authorization: Bearer core-token' \
  -H 'Content-Type: application/json' \
  -d '{"module":"core","owner":"apt-sync.py@hostA"}'
```

说明：

- `owner` 建议带主机名/实例标识，方便排查。
- `ttl_seconds` 要覆盖一次完整 `apt-sync.py` 最长运行时长。
- 如果返回 `423`，说明已有上游任务持锁，当前任务应退出或延后重试。

### 11.5 `apt-sync.py` 集成伪代码

```python
import requests

BASE = "http://127.0.0.1:8080"
TOKEN = "core-token"
MODULE = "core"
OWNER = "apt-sync.py@hostA"

headers = {
    "Authorization": f"Bearer {TOKEN}",
    "Content-Type": "application/json",
}

def acquire_lock():
    r = requests.post(
        f"{BASE}/v1/locks",
        headers=headers,
        json={"module": MODULE, "owner": OWNER, "ttl_seconds": 3600},
        timeout=10,
    )
    if r.status_code == 204:
        return True
    if r.status_code == 423:
        return False
    r.raise_for_status()

def release_lock():
    requests.post(
        f"{BASE}/v1/locks/release",
        headers=headers,
        json={"module": MODULE, "owner": OWNER},
        timeout=10,
    )

ok = acquire_lock()
if not ok:
    raise SystemExit("module is locked by another upstream sync")

try:
    # 在这里执行 apt-sync.py 的真实同步逻辑
    pass
finally:
    release_lock()
```

### 11.6 lock 文件内容（服务端内部）

服务端会在 `server.lock_dir` 下生成 lock 文件（内部实现细节，供排障参考）：

- 文件名基于锁 key（如模块锁）
- 内容包含 `owner`、过期时间、锁 key

注意：

- 这些文件由 `sync-server` 管理，不建议由外部程序直接修改。
- 运维排障时可只读查看，不要手工覆盖写入。

---

## 12. 测试

执行全量测试：

```bash
go test ./...
```

已覆盖：

- 模块配置解析与校验
- 模块鉴权
- 模块锁隔离
- WebSocket 解锁通知
- 端到端同步流程
- 删除保护触发
- 断点续传

---

## 13. 常见问题

### 12.1 提示 `module is required`

客户端必须传 `-module`。

### 12.2 提示 `token not authorized for module`

确认 token 在该模块 `tokens` 列表内。

可用下面命令快速验证：

```bash
curl -X POST http://127.0.0.1:8080/v1/sessions \
  -H 'Authorization: Bearer <你的token>' \
  -H 'Content-Type: application/json' \
  -d '{"module":"<模块名>","source_path":".","client_id":"debug"}'
```

### 12.3 同步一直等待

说明模块仍被上游任务持有锁，检查锁持有者并确保释放。

### 12.4 删除保护触发

先确认是否为异常删除；若确认合理，可调大阈值或临时使用 `-force-delete-guard`。

---

## 14. 兼容性说明

- 旧客户端（不传 `module`）不兼容当前接口语义
- 建议所有调用统一升级到模块化请求
