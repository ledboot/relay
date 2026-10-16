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

- 后台读取真实上游服务列表；选择 OpenAI、Telegram、WhatsApp 或其他提供 SMS 的平台，批量生成 1–100 枚专用 CD Key。生成时选择类型：`verification`（一次性接码）或 `nonrenewable`（不续费租用，需同时选择时长）。服务列表按类型向上游查询，同一平台可能只支持其中一种。
- 完整 CD Key 支持下载，并持久保存到数据库；仅认证后的管理列表返回完整密钥，支持查看、搜索、逐条复制和禁用。兑换仍通过 SHA-256 哈希查找，公开接口不返回完整密钥。旧记录若未保存原文则显示脱敏提示。
- 输入 CD Key 申请号码，使用号码向目标平台请求验证码；页面每 6 秒查询，Go 每 15 秒补偿同步所有等待订单及历史未区分原因的已结束订单，关闭页面后仍会更新。超时、取消、退款和举报分别保存并在前后台显示；超时以供应商返回的状态为准，不根据本地时钟推断。迟到的短信回调仍会保存验证码和原文。
- 收到非空短信或验证码后核销；重复兑换返回原订单，不再次购买。
- 申请时保存 `allocating` 状态，以密钥哈希作为固定 `Idempotency-Key`。请求级超时或数据库保存失败可再次兑换，重启后仍沿用同一购买标识。
- 上游判定接码超时（`timed_out`，即这次没有收到验证码）时 CD Key 仍然有效：再次兑换会为该密钥购买一个新号码，最多 `MAX_RETRIES` 次（默认 1，`0` 表示关闭重试）。重试使用按次数派生的 `Idempotency-Key`，中断后可续购且不会重复扣费；次数用完后返回 409，转管理员补发。重新兑换会替换 `reservation_id`，被放弃的那次订单若之后才收到短信回调，将无法再匹配到该密钥。
- 号码过期或取消且无短信标记 `expired`，不自动发起新的付费购买；由管理员核查后另发新密钥。上游拒绝申请或幂等购买过期的密钥会保留申请中状态，也需管理员核查后补发，避免在结果不明确时再次扣费。取消、退款和举报同样不自动重试。上游以 400/403/404 明确拒绝的申请（价格上限、无库存等）与非确定性的超时分开报告，前者提示联系管理员，后者提示稍后重新兑换，重试同一请求不会再失败一次。
- 并非所有平台始终有库存，平台名称由上游返回，目标平台是否接受号码由该平台决定。

## 租用（Rental）CD Key

类型为 `nonrenewable` 的 CD Key 兑换后购买一个**不续费**的上游租用号码，在有效期内一直可用；到期是正常的生命周期终点，不需要管理员处理。

- 状态机独立于接码：`ready → allocating → rental_active`，到期为 `rental_expired`，上游退款为 `rental_refunded`。租用不会进入 `waiting`、`used` 或 `timed_out`，`MAX_RETRIES` 只作用于接码。
- **短信不落库，每次响应都从上游实时读取后返回**。服务端每次查询都调一次上游的短信接口，按租用单自身的 `createdAt` 过滤（号码在此之前的旧历史不会出现），按时间倒序排列（最新的在最上面），最多返回 `maxRentalMessages`（20）条，然后丢弃。刷新、退出后重新输入密钥都能看到；数据库里没有验证码。
- 因为不落库，租用的 webhook 只做事件去重，不写任何内容；短信完全以轮询到上游的结果为准（上游「List SMS」返回的就是最新的一批）。旧版本遗留的 `messages` 表会在 `migrate()` 里 `DROP` 掉，避免已写入的验证码留在磁盘上。
- 购买流程比接码多一步：`POST /reservations/rental` 返回的是 **sale** 的 Location，需再读 `GET /sales/{id}` 才能拿到 rental ID。sale 仍在处理中时返回 502，密钥留在 `allocating`，下次兑换用同一 `Idempotency-Key` 重放同一笔 sale，不会重复扣费。
- 上游的租用创建接口**没有 `maxPrice` 字段**，因此价格上限由本服务在**首次**申请前调用 `POST /pricing/rentals` 核对。超过 `TEXTVERIFIED_MAX_RENTAL_PRICE` 时返回 409 且密钥保持 `ready`，不产生任何消费；价格查不到时返回 502 并拒绝购买（这是唯一的消费护栏，宁可失败也不冒超支风险）。该检查只在 `ready → allocating` 转换时执行一次，不会阻塞中断后的续购。
- 有效期枚举：`oneDay`、`threeDay`、`sevenDay`、`fourteenDay`、`thirtyDay`、`ninetyDay`、`oneYear`。自动续费（`renewable`）尚未开放，后台按 `renewable` 生成会被拒绝。

### 前台交互：接收会话与倒计时

租用号码能活好几天，页面不能无限轮询，所以每次兑换开启一个**有限时长的接收会话**：

- 租用：每次会话 `RENTAL_SESSION_SECONDS`（10 分钟），页面每 10 秒查询一次。
- 接码：会话跟着号码有效期（`endsAt`），最多再看 15 分钟，再宽限 `VERIFICATION_GRACE_SECONDS`（2 分钟）等上游把超时状态同步回来；页面每 6 秒查询一次。
- 两条提示分开展示：接码只显示「号码有效期剩余 mm:ss」倒计时；租用同时显示「有效期至 <日期时间>」（有效期以天计）和「本页自动接收剩余 mm:ss」。
- 会话到期后页面停止请求上游，显示「已暂停自动接收…」，并提供「继续接收」（重新取一次最新状态并开一个新窗口）与「退出」（结束本页接收，回到输入密钥的界面）。
- 「退出」只结束本页的接收：号码与状态在服务端，短信在上游，重新输入同一枚密钥即可看到。
- 倒计时用响应头的 `Date` 校正浏览器时钟：客户端时钟不准时倒计时仍然正确，而不是虚高或变成负数。

后台的补偿扫描按需分配：接码要等短信，每 15 秒扫一次进行中的接码订单；租用唯一会变的是到期，且短信由前台轮询和 webhook 写入，所以租用走 5 分钟一次的慢扫，避免为无人在等的号码反复请求上游。

## TextVerified 配置

官方文档：https://app.textverified.com/docs/api/v2

项目保存了读取当日的官方定义：`docs/textverified-openapi.json`。默认 API 根地址按官方规范为 `https://www.textverified.com`。

1. 在 TextVerified API 设置获取 API Key，填入 `TEXTVERIFIED_API_KEY` 和注册邮箱 `TEXTVERIFIED_API_USERNAME`。
2. 配置 `TEXTVERIFIED_MAX_PRICE`，单位与上游 API 一致（USD），默认每次最多 5，用于接码购买的 `maxPrice`。租用另用 `TEXTVERIFIED_MAX_RENTAL_PRICE`（默认 30），在购买前通过上游定价接口核对，因为租用创建接口没有 `maxPrice` 字段。真实兑换会向上游购买号码并消耗账户余额。
3. 部署 HTTPS 域名，将 `PUBLIC_ORIGIN` 设为实际访问来源，例如 `https://sms.example.com`，末尾不加斜杠。
4. 在 https://app.textverified.com/app/api/configure/webhooks 配置回调 URL：`https://sms.example.com/api/webhooks/textverified`，订阅 `v2.sms.received`。
5. 将平台给出的完整 `whsec_...` secret 写入 `TEXTVERIFIED_WEBHOOK_SECRET`，重启 API。

回调按原始请求体计算 HMAC-SHA512，验证 `X-Webhook-Signature: HMAC-SHA512=<Base64>`，按 idempotencyKey（缺省用事件 ID）在 SQLite 事务内去重并核销。订单尚未绑定时返回 503，让上游重试，避免丢失早到的短信。未知事件直接忽略。验签失败不会更新数据库。未配置 secret 返回 503。

## Docker 部署

```bash
docker compose up -d --build
```

服务名与容器名均为 `relay-api` 和 `relay-web`，数据卷显式命名为 `relay-data`（`compose.yaml` 里用 `name:` 固定，所以改目录名或 Compose 项目名都不会换成新卷）。改名后旧容器会变成孤儿并继续占用 `127.0.0.1:3000`，所以先按旧项目名 `docker compose -p <旧项目名> down --remove-orphans`，再 `up -d --build`；**不要加 `-v`**，否则会一并删除数据卷。旧卷里已有数据时，先拷到新卷再启动：`docker volume create relay-data && docker run --rm -v <旧卷>:/from -v relay-data:/to alpine sh -c 'cp -a /from/. /to/ && chown -R 10001:10001 /to'`。**必须改属主**：api 容器以 UID 10001 运行，只 `cp -a` 会留下 root 属主，SQLite 将无法写入 WAL。

前端绑定宿主机 `127.0.0.1:3000`；API 不发布端口，数据库持久化到 `relay-data` 卷。使用已有反向代理提供 HTTPS 并转发到前端。代理请求体限制建议 64 KiB，读取超时至少 60 秒。只有反向代理**覆盖而不是直接信任客户端**的 `X-Forwarded-For` 时才启用 `TRUST_PROXY=true`；否则所有请求共用一个客户端身份。用户接口不受此影响，见下面的限流说明。生产环境不要公开 Go 端口，避免绕过可信代理边界。

限流分两层。`/api/redeem` 和 `/api/status` 按 **CD Key 哈希**分桶，额度由 `KEY_RATE_LIMIT_PER_MIN` 控制（默认 120）；前端每 6 秒轮询一次约消耗 10，因此每枚密钥有十倍余量，且移动运营商 CGNAT 下共用出口 IP 的客户互不干扰。其余请求（管理接口、读不到密钥的请求、以及超过身份表上限后新出现的身份）按客户端身份分桶，额度由 `RATE_LIMIT_PER_MIN` 控制（默认 120）。身份表上限 50000，超出后新身份并入客户端桶，避免随机密钥撑爆内存。以每分钟 120 次、每次轮询 6 秒计算，旧的全站共享策略只能支持约 12 个同时等待短信的客户，因此不要把用户接口改回按客户端身份限流。修改后重启 API，或 `docker compose up -d --force-recreate`。

环境配置更新后执行 `docker compose up -d --force-recreate`。备份 SQLite 时停止 API 或使用 SQLite 在线备份命令，不能只复制活跃 WAL 数据库主文件。验证码属于敏感数据，应限制数据库备份访问并制定数据保留周期。

## API

| 接口 | 用途 |
| --- | --- |
| `POST /api/redeem` | `{ "key": "SMS-..." }` 兑换 / 恢复订单 |
| `POST /api/status` | 同上，查询订单与短信 |
| `GET /api/admin/services` | 可选平台，`?type=verification` 或 `?type=nonrenewable`（默认 verification） |
| `GET /api/admin/keys` | 分页密钥列表（page、pageSize、search、state） |
| `POST /api/admin/keys` | `{ "service": "上游真实名称", "count": 10, "brandPrefix": "RELAY", "type": "verification\|nonrenewable", "duration": "thirtyDay" }`（`duration` 仅租用需要） |
| `POST /api/admin/disable` | `{ "id": 1 }` 禁用未兑换密钥 |
| `POST /api/webhooks/textverified` | 上游签名回调 |

管理接口要求 `Authorization: Bearer <ADMIN_TOKEN>`。用户接口将 CD Key 作为访问凭据，避免把密钥放在 URL 中。没有用户注册或支付系统。`/api/redeem` 与 `/api/status` 的响应包含 `reservationType` 与 `duration`，前台据此区分接码与租用；接码响应带单个 `code` / `message`，租用响应带 `messages`（从上游实时读出，最新的在前，最多 20 条），两者的 `phone` 与 `endsAt` 都可用。

## 数据库结构

`keys` 表的关键列：`reservation_type`（`verification` / `nonrenewable` / `renewable`）、`duration`（租用时长，接码为空）、`reservation_id`（上游订单 ID，接码与租用共用同一 ID 空间，因此共用一列和 `reservation_unique` 部分唯一索引）、`state`、`phone`、`code`、`message`、`ends_at`、`retries`。`code` / `message` 只服务于接码（核销必需）；**租用的短信不存任何表**，所以库里没有租用验证码。

`openDB` 内联迁移可直接升级旧库：`verification_id` 会重命名为 `reservation_id`（索引同步重建为 `reservation_unique`），并补齐 `reservation_type`、`duration`、`full_key`、`retries`。旧记录默认 `reservation_type='verification'`。

## 验证

```bash
cd backend
go test -race ./...
go vet ./...
cd ../web
bun run build
```

测试覆盖并发兑换幂等、重复兑换、超时重新兑换与重试上限、按 CD Key 分桶的限流与身份表上限、查询核销、webhook 验签与去重、禁用及后台鉴权、上游鉴权和 Location/SMS 协议；租用另覆盖类型/时长校验、购买流程与 sale 跟随、有效期状态推进、价格上限拦截、短信按时间倒序与按租用时间过滤、条数上限、确认库里没有 `messages` 表、webhook 不存不核销，以及接码仍保持单条 `code`。测试使用本地模拟上游，不消耗 TextVerified 余额。真实 API 购买、短信到达和公网 webhook 需要配置实际凭据及域名后联调。

### 当前验证记录

2026-10-14：已迁移为 Bun 1.3.5，保留原锁定依赖版本。`bun install --frozen-lockfile` 与 `bun run build` 已通过（包括 TypeScript 检查和页面预渲染）。重装修复了早先磁盘不足留下的不完整依赖。Gin 迁移后的 Go 测试此前仍受磁盘不足阻断；Docker 镜像构建、浏览器验收、真实购买及公网回调尚未验证。

2026-10-16：新增接码超时后重新兑换（`MAX_RETRIES`）。`go test -race ./...` 与 `go vet ./...` 已可运行，新增重试用例全部通过；`retries` 列迁移已在开发库副本上验证（35 条历史记录全部默认为 0）。遗留失败 1 例：`TestPurchaseRejection` 断言的错误文案「上游拒绝」在当前源码中不存在（`redeem` 返回的是「号码申请暂未完成，请稍后重新兑换」），属于迁移到 Gin 之前的旧断言，与本次改动无关，待确认后修正。同日还把限流从「全站共享客户端身份」改为「用户接口按 CD Key 分桶」，额度可配置（`KEY_RATE_LIMIT_PER_MIN` / `RATE_LIMIT_PER_MIN`），新增 8 个限流用例；并用变异测试确认回归用例能复现旧策略下「第 13 个客户 429」的行为。同日将 Compose 服务名与容器名改为 `relay-api` / `relay-web`（`API_INTERNAL_URL` 与 `depends_on` 同步，卷名 `sms-data` 有意不改），已用 `docker compose config` 校对、`docker compose build` 构建两个镜像均成功、并在 compose 网络内实测 `relay-web` 按服务名请求 `http://relay-api:8080/api/health` 返回 200。随后把项目目录、Go module（`module relay`）和前端包名（`relay-web`）统一为 relay，数据卷用 `name:` 固定为 `relay-data`。浏览器验收、真实购买及公网回调仍未验证。

2026-10-16：实现租用（`nonrenewable`）CD Key。`keys` 增加 `reservation_type` / `duration`，`verification_id` 重命名为 `reservation_id`，状态机按类型分叉（`rental_active` / `rental_expired` / `rental_refunded`），后台可按类型生成并选择 7 种时长。租用只保存号码与有效期、不保存短信内容。`go test -race ./...`、`go vet ./...`、`bun run build` 全部通过；同时修复上一条遗留的 `TestPurchaseRejection`：上游 400/403/404 现在返回「上游拒绝本次号码申请（<code>），请联系管理员处理」，只有非确定性失败才提示稍后重试。真实租用购买、租用到期与退款、以及租用回调尚未用真实凭据联调。（同日稍后已推翻「不保存短信内容」这一条，见下。）

2026-10-16：租用改为保存并展示全部短信。新增 `messages` 表（`key_id` + `dedup_key` 唯一，时间统一存 UTC），轮询与 webhook 都追加写入并去重，用户接口返回 `messages`（最新在前，最多 50 条）。前台重做为「接收会话」：租用 10 分钟、接码跟随 `endsAt` 加 2 分钟宽限，会话到期停止轮询，提供继续接收与退出；接码的有效期从绝对时间改为倒计时，倒计时用响应头 `Date` 校正浏览器时钟。验证方式：`go test -race ./...`（含新增的短信倒序/去重/webhook 追加用例）、`go vet ./...`、`bun run build` 全部通过；另用模拟上游跑了一遍端到端断言，并实际在浏览器里验收了租用面板（号码、3 条短信倒序、有效期、会话倒计时、退出）、接码倒计时、以及会话到期后的暂停/继续。真实凭据下的租用购买、到期与公网回调仍待联调。（同日再次调整：短信改为不落库，见下。）

2026-10-16：租用短信改为实时读取、不落库。删掉 `messages` 表（`migrate()` 里 `DROP TABLE IF EXISTS messages`，把之前存进去的验证码一并清掉），每次查询都从上游取实时列表，按租用单自身的 `createdAt` 过滤掉号码之前的历史，按时间倒序、最多 20 条返回后丢弃；webhook 对租用只做事件去重，不写任何内容。前台只改了一句暂停文案（短信在号码上，不在服务器）。验证方式：`go test -race -count=1 ./...` 与 `go vet ./...` 通过，新增用例断言短信按时间倒序、早于租用的历史被过滤、超过上限被截断、**库里不存在 `messages` 表**、未成功的租用仍能返回短信；另用模拟上游重跑了端到端断言。真实凭据下的租用购买、到期与公网回调仍待联调。

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
