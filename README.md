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

## 11. 测试

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

## 12. 常见问题

### 12.1 提示 `module is required`

客户端必须传 `-module`。

### 12.2 提示 `token not authorized for module`

确认 token 在该模块 `tokens` 列表内。

### 12.3 同步一直等待

说明模块仍被上游任务持有锁，检查锁持有者并确保释放。

### 12.4 删除保护触发

先确认是否为异常删除；若确认合理，可调大阈值或临时使用 `-force-delete-guard`。

---

## 13. 兼容性说明

- 旧客户端（不传 `module`）不兼容当前接口语义
- 建议所有调用统一升级到模块化请求
