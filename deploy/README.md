# deploy/ — 上云部署件（C-01 ~ C-10）

本目录与仓库根的部署文件一起构成 T4 事实表 §4.5「上云前必须改」的落地物。
改动依据全部来自 `facts/T4-合规落地-事实表.md`，未新增事实表以外的结论。

## 文件

| 文件 | 对应项 | 作用 |
|---|---|---|
| `../docker-compose.yml` | C-03 / C-04 / C-06 / C-09 | 生产形态 compose：自建镜像、无宿主直映射、无 container_name、无弱口令、资源上限、容器日志轮转、显式 TRUSTED_PROXIES、健康检查走 /api/readyz |
| `../Dockerfile` | C-04 | 运行阶段以 UID/GID 10001 非 root 运行，/data 唯一可写目录 |
| `../new-api.service` | C-05 | systemd unit：EnvironmentFile + *_FILE 密钥、TimeoutStopSec=130、LimitNOFILE、最小权限 |
| `env.production.example` | C-03 / C-05 / C-09 | 环境变量模板，强制项缺失即启动失败 |
| `logrotate/new-api` | C-06 | 只在「关掉程序内轮转、交给系统 logrotate」时使用 |
| `../common/secret_source.go` | C-01 | `*_FILE` 密钥读取（SESSION_SECRET_FILE / CRYPTO_SECRET_FILE） |
| `../controller/health_probe.go` | C-02 | `GET /api/healthz`（liveness）与 `GET /api/readyz`（readiness，校验 DB，匿名可访问） |
| `../logger/logfile.go` | C-06 | 按大小轮转 + 按天数清理日志 |
| `../model/migration_version.go` | C-08 | schema_migrations 版本表 + 迁移登记框架（不含自动下行） |

## 探针怎么配

| 用途 | 路径 | 期望 | 说明 |
|---|---|---|---|
| liveness（进程活着） | `GET /api/healthz` | 200 恒返回 | 不查依赖，避免 DB 抖动触发重启风暴 |
| readiness（能服务） | `GET /api/readyz` | DB 通 200；DB 不通 503 | 匿名、无 AdminAuth；响应体 `checks.database` / `checks.redis` |
| 不要用 | `GET /api/status` | 恒 `success:true` | 不校验 DB（T4 §3.3），只能当「进程在」用 |

`READINESS_REQUIRE_REDIS=true` 可把 Redis 变硬依赖（默认 false，与代码里 Redis 可选一致）。

## 密钥怎么放（C-01）

```bash
mkdir -p secrets && chmod 700 secrets
openssl rand -hex 32 > secrets/session_secret
openssl rand -hex 32 > secrets/crypto_secret
chmod 400 secrets/* && chown 10001:10001 secrets/*
```

- 只设 `SESSION_SECRET_FILE` / `CRYPTO_SECRET_FILE` 即可启动；同时设了环境变量时以文件为准并打告警（不会沉默用一个你不知道的值）。
- 文件权限若是组/其他人可读会打告警；建议 0400。
- `SESSION_SECRET=random_string` 仍然直接拒启（启动校验保留）。
- 换 SESSION_SECRET = 全员会话与 refresh token 失效，先备份再更换。

## 日志（C-06）

- 目录：容器 `/data/logs`，裸机 `/var/lib/new-api/logs`（与数据盘同盘，T4 §4.5 #7）。
- 程序自身：`LOG_MAX_SIZE_MB`（默认 200）、`LOG_RETENTION_DAYS`（默认 14）、`LOG_MAX_BACKUPS`（默认 0=不按个数限制）。
- 程序轮转与系统 logrotate **二选一**，双开会在同一批文件上按不同规则切割。

## 回滚（C-08 / V2-3-17）

本仓仍然**没有**自动下行迁移。升级前后的可行回滚：

| 层 | 手段 | 边界 |
|---|---|---|
| L1 二进制 | 换回上一版 `new-api` 二进制 | 仅当库还是旧 schema 时成立 |
| L2 schema | **无自动下行**，只能恢复备份/快照 | 新版 AutoMigrate 已加过的列/索引不会自动消失 |
| L3 数据 | RDS 时间点恢复（PITR）或 ECS 快照 | 代码侧零实现，全靠云侧（T4 §3.1/§3.2） |

硬事实（演练时会先撞到的）：**新二进制遇到旧的 32 位额度 schema 会直接拒绝启动**
（`model/main.go ensureUserQuotaColumns`，可用 `SKIP_64BIT_QUOTA_SCHEMA_CHECK=true` 跳过，
但跳过即不保证正确，不是兼容路径）。

升级后 `schema_migrations` 表会留下轨迹：

```sql
SELECT version, description, applied_at, app_version, destructive FROM schema_migrations ORDER BY version;
```

`destructive=true` 的行表示「退回该版本之前的状态必须靠恢复，而不是脚本」——
这是给回滚决策用的标记，不是可执行的下行脚本。
