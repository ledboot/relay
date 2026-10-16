# Relay / SMS Verified

Go + Gin + SQLite 后端，Next.js 中文兑换前台与管理后台，对接 TextVerified API v2。前台 `/`，管理后台路径由 `ADMIN_PATH` 配置（默认 `/console`），原 `/admin` 页面不可访问。

## 本地启动

需要 Go 1.24+、Node.js 22+、Bun 1.3.5。Bun 管理前端依赖和运行脚本，Next.js 生产服务仍使用 Node.js。

```bash
cp .env.example .env
# 编辑 .env，填入管理密钥、TextVerified API 用户邮箱与 API Key
cd web
bun install --frozen-lockfile
cd ..
./scripts/start-go.sh
```

另开终端运行 `./scripts/start-web.sh`，浏览器打开 http://localhost:3000 。管理后台使用 `.env` 中的 `ADMIN_TOKEN` 登录。管理密钥仅保留在当前页面内存中，刷新后重新输入。可用 `openssl rand -hex 32` 生成密钥。不要保留示例值。

## 功能

- 后台读取真实上游服务列表；选择 OpenAI、Telegram、WhatsApp 或其他提供 SMS 的平台，批量生成 1–100 枚专用 CD Key。
- 完整 CD Key 支持下载，并持久保存到数据库；仅认证后的管理列表返回完整密钥，支持查看、搜索、逐条复制和禁用。兑换仍通过 SHA-256 哈希查找，公开接口不返回完整密钥。旧记录若未保存原文则显示脱敏提示。
- 输入 CD Key 申请号码，使用号码向目标平台请求验证码；页面每 6 秒查询，Go 每 15 秒补偿同步所有等待订单及历史未区分原因的已结束订单，关闭页面后仍会更新。超时、取消、退款和举报分别保存并在前后台显示；超时以供应商返回的状态为准，不根据本地时钟推断。迟到的短信回调仍会保存验证码和原文。
- 收到非空短信或验证码后核销；重复兑换返回原订单，不再次购买。
- 申请时保存 `allocating` 状态，以密钥哈希作为固定 `Idempotency-Key`。请求级超时或数据库保存失败可再次兑换，重启后仍沿用同一购买标识。
- 上游判定接码超时（`timed_out`，即这次没有收到验证码）时 CD Key 仍然有效：再次兑换会为该密钥购买一个新号码，最多 `MAX_RETRIES` 次（默认 1，`0` 表示关闭重试）。重试使用按次数派生的 `Idempotency-Key`，中断后可续购且不会重复扣费；次数用完后返回 409，转管理员补发。重新兑换会替换 `verification_id`，被放弃的那次订单若之后才收到短信回调，将无法再匹配到该密钥。
- 号码过期或取消且无短信标记 `expired`，不自动发起新的付费购买；由管理员核查后另发新密钥。上游拒绝申请或幂等购买过期的密钥会保留申请中状态，也需管理员核查后补发，避免在结果不明确时再次扣费。取消、退款和举报同样不自动重试。
- 并非所有平台始终有库存，平台名称由上游返回，目标平台是否接受号码由该平台决定。

## TextVerified 配置

官方文档：https://app.textverified.com/docs/api/v2

项目保存了读取当日的官方定义：`docs/textverified-openapi.json`。默认 API 根地址按官方规范为 `https://www.textverified.com`。

1. 在 TextVerified API 设置获取 API Key，填入 `TEXTVERIFIED_API_KEY` 和注册邮箱 `TEXTVERIFIED_API_USERNAME`。
2. 配置 `TEXTVERIFIED_MAX_PRICE`，单位与上游 API 一致（USD），默认每次最多 5。真实兑换会向上游购买号码并消耗账户余额。
3. 部署 HTTPS 域名，将 `PUBLIC_ORIGIN` 设为实际访问来源，例如 `https://sms.example.com`，末尾不加斜杠。
4. 在 https://app.textverified.com/app/api/configure/webhooks 配置回调 URL：`https://sms.example.com/api/webhooks/textverified`，订阅 `v2.sms.received`。
5. 将平台给出的完整 `whsec_...` secret 写入 `TEXTVERIFIED_WEBHOOK_SECRET`，重启 API。

回调按原始请求体计算 HMAC-SHA512，验证 `X-Webhook-Signature: HMAC-SHA512=<Base64>`，按 idempotencyKey（缺省用事件 ID）在 SQLite 事务内去重并核销。订单尚未绑定时返回 503，让上游重试，避免丢失早到的短信。未知事件直接忽略。验签失败不会更新数据库。未配置 secret 返回 503。

## Docker 部署

```bash
docker compose up -d --build
```

服务名与容器名均为 `relay-api` 和 `relay-web`。从旧的 `api` / `web` 改名后，旧容器会变成孤儿并继续占用 `127.0.0.1:3000`，所以要先 `docker compose down --remove-orphans` 再 `up -d --build`；**不要加 `-v`**，否则会一并删除数据卷。卷名有意保持 `sms-data` 不变，改名不影响已有数据；不要顺手改成 `relay-data`，那会新建一个空卷。

前端绑定宿主机 `127.0.0.1:3000`；API 不发布端口，数据库持久化到 `sms-data` 卷。使用已有反向代理提供 HTTPS 并转发到前端。代理请求体限制建议 64 KiB，读取超时至少 60 秒。只有反向代理**覆盖而不是直接信任客户端**的 `X-Forwarded-For` 时才启用 `TRUST_PROXY=true`；否则所有请求共用一个客户端身份。用户接口不受此影响，见下面的限流说明。生产环境不要公开 Go 端口，避免绕过可信代理边界。

限流分两层。`/api/redeem` 和 `/api/status` 按 **CD Key 哈希**分桶，额度由 `KEY_RATE_LIMIT_PER_MIN` 控制（默认 120）；前端每 6 秒轮询一次约消耗 10，因此每枚密钥有十倍余量，且移动运营商 CGNAT 下共用出口 IP 的客户互不干扰。其余请求（管理接口、读不到密钥的请求、以及超过身份表上限后新出现的身份）按客户端身份分桶，额度由 `RATE_LIMIT_PER_MIN` 控制（默认 120）。身份表上限 50000，超出后新身份并入客户端桶，避免随机密钥撑爆内存。以每分钟 120 次、每次轮询 6 秒计算，旧的全站共享策略只能支持约 12 个同时等待短信的客户，因此不要把用户接口改回按客户端身份限流。修改后重启 API，或 `docker compose up -d --force-recreate`。

环境配置更新后执行 `docker compose up -d --force-recreate`。备份 SQLite 时停止 API 或使用 SQLite 在线备份命令，不能只复制活跃 WAL 数据库主文件。验证码属于敏感数据，应限制数据库备份访问并制定数据保留周期。

## API

| 接口 | 用途 |
| --- | --- |
| `POST /api/redeem` | `{ "key": "SMS-..." }` 兑换 / 恢复订单 |
| `POST /api/status` | 同上，查询订单与短信 |
| `GET /api/admin/services` | 可选平台 |
| `GET /api/admin/keys` | 分页密钥列表（page、pageSize、search、state） |
| `POST /api/admin/keys` | `{ "service": "上游真实名称", "count": 10, "brandPrefix": "RELAY" }` |
| `POST /api/admin/disable` | `{ "id": 1 }` 禁用未兑换密钥 |
| `POST /api/webhooks/textverified` | 上游签名回调 |

管理接口要求 `Authorization: Bearer <ADMIN_TOKEN>`。用户接口将 CD Key 作为访问凭据，避免把密钥放在 URL 中。没有用户注册或支付系统。

## 验证

```bash
cd backend
go test -race ./...
go vet ./...
cd ../web
bun run build
```

测试覆盖并发兑换幂等、重复兑换、超时重新兑换与重试上限、按 CD Key 分桶的限流与身份表上限、查询核销、webhook 验签与去重、禁用及后台鉴权、上游鉴权和 Location/SMS 协议。测试使用本地模拟上游，不消耗 TextVerified 余额。真实 API 购买、短信到达和公网 webhook 需要配置实际凭据及域名后联调。

### 当前验证记录

2026-10-14：已迁移为 Bun 1.3.5，保留原锁定依赖版本。`bun install --frozen-lockfile` 与 `bun run build` 已通过（包括 TypeScript 检查和页面预渲染）。重装修复了早先磁盘不足留下的不完整依赖。Gin 迁移后的 Go 测试此前仍受磁盘不足阻断；Docker 镜像构建、浏览器验收、真实购买及公网回调尚未验证。

2026-10-16：新增接码超时后重新兑换（`MAX_RETRIES`）。`go test -race ./...` 与 `go vet ./...` 已可运行，新增重试用例全部通过；`retries` 列迁移已在开发库副本上验证（35 条历史记录全部默认为 0）。遗留失败 1 例：`TestPurchaseRejection` 断言的错误文案「上游拒绝」在当前源码中不存在（`redeem` 返回的是「号码申请暂未完成，请稍后重新兑换」），属于迁移到 Gin 之前的旧断言，与本次改动无关，待确认后修正。同日还把限流从「全站共享客户端身份」改为「用户接口按 CD Key 分桶」，额度可配置（`KEY_RATE_LIMIT_PER_MIN` / `RATE_LIMIT_PER_MIN`），新增 8 个限流用例；并用变异测试确认回归用例能复现旧策略下「第 13 个客户 429」的行为。同日将 Compose 服务名与容器名改为 `relay-api` / `relay-web`（`API_INTERNAL_URL` 与 `depends_on` 同步，卷名 `sms-data` 有意不改），已用 `docker compose config` 校对、`docker compose build` 构建两个镜像均成功、并在 compose 网络内实测 `relay-web` 按服务名请求 `http://relay-api:8080/api/health` 返回 200。浏览器验收、真实购买及公网回调仍未验证。

## CDK 前缀

新密钥格式：`品牌前缀-服务前缀-随机密钥`，例如 `RELAY-OPENAI-<48位随机十六进制字符>`。生成时可填写品牌前缀（1–16 位英文字母或数字，统一大写，API 省略时默认为 RELAY）；服务前缀取所选上游服务名的大写字母和数字，最多 24 位。服务仍以数据库绑定为准，前缀仅用于识别。后台列表显示完整密钥。已有 `SMS-...` 密钥继续可用。

## HTTP 与页面渲染

Go 使用 Gin 路由及管理接口鉴权中间件，只提供 JSON API。为兼容本项目 Go 1.24 工具链，使用 Gin 1.11 系列。Next.js 使用 App Router 和 standalone Node.js 服务；根布局为服务端组件，兑换页及管理页为客户端组件，初始页面可预渲染，业务数据通过浏览器 fetch 获取。Next.js 的 `/api/*` Route Handler 在服务端转发请求给 Go，因此当前不是纯静态导出，也不是 Go 模板渲染或每次请求服务端获取业务数据的 SSR。

## 前端依赖管理

统一使用 Bun 1.3.5，提交 `web/bun.lock`，不再维护 `package-lock.json`。首次安装和 CI 使用 `bun install --frozen-lockfile`；新增依赖使用 `bun add <包名>`，更新依赖后同时提交 package.json 与 bun.lock。开发使用 `./scripts/start-web.sh`，或在 web 目录执行 `bun run dev`。Docker 构建使用同版本 Bun 安装依赖及执行构建脚本，运行阶段仍采用 Node.js。

本地开发启用 Next.js Fast Refresh，使用 `.next-dev` 缓存目录；`bun run build` 的生产产物仍位于 `.next`，避免构建覆盖正在运行的开发缓存。修改页面后会自动更新；如修改配置需要重新启动，使用 `./scripts/start-web.sh`。

管理页面地址：在根目录 `.env` 设置 `ADMIN_PATH=/your-console`，支持多级路径（如 `/ops/console`）。路径必须以 `/` 开头，仅使用英文字母、数字、下划线和连字符；不能使用 `/admin`、`/api`、`/_next` 等保留入口。修改后重启前端；Docker 环境重新创建 web 容器即可，无需重新构建镜像。管理 API 仍使用 `/api/admin/*` 并校验管理密钥。

## 后端日志

统一使用 Go 标准库 `log/slog`，输出到标准输出。`LOG_FORMAT=text` 为易读文本，`json` 为结构化 JSON；`LOG_LEVEL` 支持 `DEBUG`、`INFO`（默认）、`WARN`、`ERROR`。未设置格式时本地默认 text，Docker Compose 默认 json；显式环境配置优先。修改配置后重启后端。

请求日志包含服务端生成的请求 ID、方法、路由模板、状态码、耗时和 panic 标记，覆盖鉴权失败、限流及未知接口。响应头返回 `X-Request-ID`。不记录请求正文、查询参数、请求头、响应内容或 panic 内容；未知路径统一记录为 unmatched，业务异常仅记录错误类型，避免上游错误携带敏感信息。日志级别过滤同时适用于请求日志和业务日志。

Docker 中通过 `docker compose logs -f relay-api` 查看；文件保存、滚动和保留期限由部署环境配置。
