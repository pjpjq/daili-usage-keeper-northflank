---
title: Daili Usage Keeper
emoji: 📊
colorFrom: indigo
colorTo: blue
sdk: docker
app_port: 8080
pinned: false
---

# Daili Usage Keeper

[English README](./README.en.md)

`Daili Usage Keeper` 是一个独立的用量持久化与可视化服务。这个 Space 只提供受密码保护的统计看板，不提供模型推理、API 代理或请求转发服务。

Space 的 Docker 镜像由当前仓库中的 Go 与 React 源码直接构建，最终运行在标准 Alpine 镜像上。服务从单独配置的 management metadata 端点读取用量数据，写入 SQLite，并提供 usage、pricing、request health 和 model/API 维度的统计视图。

![cpa-usage-keeper-screenshot](https://images.bitskyline.com/i/2026/05/1pmg6l.png)

## 功能特性

- CPA usage 数据持久化到 SQLite
- usage 聚合 API 与 pricing API
- 启动时及每 6 小时从 models.dev 同步当前 CPA 模型价格（可配置，失败保留旧价格）
- 内置 React Dashboard
- 可选密码登录保护
- SQLite 数据库本地备份与保留策略
- Docker / Docker Compose 部署

## Vercel 前端部署

React 看板可以独立部署到 Vercel。配置文件
[`web/vercel.json`](./web/vercel.json) 将 `/api/v1/*` 请求代理到独立运行的后端，
因此浏览器不需要跨域访问 API。

- [一键部署前端到 Vercel](https://vercel.com/new/clone?repository-url=https://github.com/pjpjq/daili-usage-keeper-northflank&root-directory=web)
- [在线看板](https://daili-usage-keeper-northflank.vercel.app)
- 后端健康检查：<https://p01--daili-usage--q29tm9z7cs9k.code.run/healthz>

Vercel 部署只包含前端；Go API、SQLite 数据库和后台同步仍运行在独立的
Northflank 服务中。公开看板前，请在后端设置密码（`AUTH_ENABLED=true`、
`LOGIN_PASSWORD=...`）。不要提交后端密钥、密码或运行时数据。

Thanks to Vercel for their support of open-source software,

## 项目结构

```text
cmd/                 应用入口
internal/api/        HTTP 路由与处理器
internal/app/        应用装配与启动
internal/auth/       内存 session 鉴权
internal/backup/     SQLite 数据库备份管理
internal/config/     环境配置加载
internal/cpa/        CPA 客户端与类型定义
internal/models/     GORM 模型
internal/poller/     后台同步轮询
internal/repository/ SQLite 访问与聚合逻辑
internal/service/    同步、usage 与 pricing 服务
web/                 React + TypeScript 前端
```

## 配置

复制配置模板：

```bash
cp .env.example .env
```

| 变量 | 必填 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `CPA_BASE_URL` | 是 | - | CPA 服务地址 |
| `CPA_MANAGEMENT_KEY` | 是 | - | CPA management key |
| `AUTH_ENABLED` | 否 | `false` | 是否启用登录保护 |
| `LOGIN_PASSWORD` | 鉴权启用时必填 | - | 登录密码 |
| `AUTH_SESSION_TTL` | 否 | `168h` | Session 生命周期 |
| `APP_PORT` | 否 | `8080` | HTTP 监听端口 |
| `APP_BASE_PATH` | 否 | 根路径 | 子路径部署前缀，例如 `/cpa`；留空表示 `/` |
| `TZ` | 否 | `Asia/Shanghai` | 项目业务时区，影响 Today、按天聚合、定时任务和日志时间 |
| `REDIS_QUEUE_ADDR` | 否 | `CPA_BASE_URL` 主机名 + `8317` | CPA Redis/RESP TCP 地址；非默认端口时填写 `host:port` |
| `REDIS_QUEUE_BATCH_SIZE` | 否 | `1000` | 每次最多拉取的队列记录数 |
| `REDIS_QUEUE_IDLE_INTERVAL` | 否 | `1s` | 队列为空时的检查间隔 |
| `METADATA_SYNC_INTERVAL` | 否 | `30s` | auth files 与 provider 元数据刷新间隔 |
| `REQUEST_TIMEOUT` | 否 | `30s` | CPA 请求超时 |
| `PRICING_SYNC_ENABLED` | 否 | `true` | 是否启用第三方模型价格定时同步 |
| `PRICING_SYNC_INTERVAL` | 否 | `6h` | 模型价格同步间隔 |
| `PRICING_SOURCE_URL` | 否 | `https://models.dev/api.json` | 第三方模型价格目录 |
| `WORK_DIR` | 否 | `./data` | 应用工作目录；数据库、日志和备份默认分别写入 `app.db`、`logs/`、`backups/` |
| `LOG_LEVEL` | 否 | `info` | 日志级别 |
| `LOG_FILE_ENABLED` | 否 | `true` | 是否写入持久化日志文件 |
| `LOG_RETENTION_DAYS` | 否 | `7` | 日志保留天数；`0` 表示不自动清理 |
| `BACKUP_ENABLED` | 否 | `true` | 是否启用 SQLite 数据库备份 |
| `BACKUP_INTERVAL` | 否 | `24h` | 数据库备份间隔 |
| `BACKUP_RETENTION_DAYS` | 否 | `7` | 备份保留天数 |

`APP_BASE_PATH` 必须为空或以 `/` 开头；例如 `/cpa`，`/cpa/` 会规范为 `/cpa`。

安全与数据说明：

- SQLite 数据库备份会保存应用数据库中的原始数据，备份文件不做加密。
- 面向浏览器的 API 会对 key-like source/lookup 字段做脱敏或稳定公开标识映射，但不会修改数据库原始值。
- 公开部署建议开启 `AUTH_ENABLED=true`，并在反向代理层配置 HTTPS。
- 登录 session 存在服务进程内存中，服务重启后已登录 session 会失效。
- Redis inbox 原始消息会自动清理：成功数据保留到当天结束后清理，失败数据保留 7 天。

## 本地开发

### 前置依赖

- Go 1.22+
- Node.js 22+
- npm
- 已运行的 [CLIProxyAPI（CPA）](https://github.com/router-for-me/CLIProxyAPI)

### 本地启动

1. 复制本地配置：

```bash
cp .env.example .env
```

2. 启动后端：

```bash
go run ./cmd/server/main.go
```

3. 在另一个终端安装前端依赖并启动开发服务器：

```bash
npm --prefix ./web ci
npm --prefix ./web run dev -- --host 127.0.0.1
```

4. 构建前端生产产物：

```bash
npm --prefix ./web run build
```

### 测试

运行完整的本地验证基线：

```bash
make verify
```

也可以单独运行各项检查：

```bash
go test ./cmd/... ./internal/...
npm --prefix ./web run test
npm --prefix ./web run lint
npm --prefix ./web run typecheck
npm --prefix ./web run build
```

## Docker

如果 CPA 已在宿主机运行：

```bash
# TZ 设置容器时区，日志时间会按该时区显示。
docker run -d \
  --name cpa-usage-keeper \
  --add-host=host.docker.internal:host-gateway \
  -p 8080:8080 \
  -v "$(pwd)/keeper/data:/data" \
  -e TZ=Asia/Shanghai \
  -e CPA_BASE_URL=http://host.docker.internal:8317 \
  -e CPA_MANAGEMENT_KEY=replace-with-your-management-key \
  -e REDIS_QUEUE_ADDR=host.docker.internal:8317 \
  -e AUTH_ENABLED=true \
  -e LOGIN_PASSWORD=replace-with-your-login-password \
  ghcr.io/willxup/cpa-usage-keeper:latest
```

`/data` 用于保存 SQLite 数据库、备份文件和日志文件，请挂载到持久化目录。

## 子路径反代

部署到 `/cpa` 时设置 `APP_BASE_PATH=/cpa`，并在反向代理中保留该前缀：

```nginx
location /cpa/ {
    proxy_pass http://127.0.0.1:8080;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
}
```

## Daili Space deployment

This Space builds the usage dashboard from source and imports usage metadata from the separately configured `CPA_BASE_URL`. Dashboard auth is enabled with `LOGIN_PASSWORD`.
