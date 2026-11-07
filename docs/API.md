# 万界零方-中心hub API 一览

约定：`Authorization: Bearer <token>` 或 `mcai_token` Cookie；卫星站接口另需 `X-Site-Key`；worker 接口需 `X-Worker-Key`。
错误统一为 `{"error":"code","message":"..."}`，状态码语义：401 未登录 / 402 余额不足 / 403 无权 / 404 不存在 / 409 冲突 / 429 限流 / 503 维护。

## 用户与登录

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/register` | `{email,username,password,invite_code?}`，新账号余额 0（赠送延后到首单成功） |
| POST | `/api/login` | `{identifier,password}`，identifier 可为邮箱或用户名 |
| POST | `/api/logout` | 清 Cookie |
| GET | `/api/me` | 本人信息 + 余额 + 邀请码 |
| GET | `/api/v1/me/overview` | 用户中心聚合（余额/流水/任务统计/订阅/CDK/菜单） |
| GET | `/auth/login` `/auth/register` | 托管认证页（hub 渲染 HTML，卫星站只跳转） |

## SSO

```
GET  /sso?redirect=<白名单内地址>   → 302 到 redirect?code=xxx（未登录先跳 /auth/login）
POST /api/sso/exchange {code}       → {token,user_id,redirect}，code 60 秒有效、一次性
```

## 配置

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/config` | 公开配置（brand/billing/catalog/features/limits + config_version） |
| GET | `/api/v1/site/config` | 站点配置：需 `X-Site-Key`；支持 `If-None-Match`(ETag)、`?since=N`(未变 304)、`?full=1` |
| GET | `/api/admin/config` | 全部原始分区（admin） |
| POST | `/api/admin/config` | `{section,values}` → 版本 +1，热生效；改 `catalog.mc_versions` 自动生成适配任务 |

## 钱包与支付

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/redeem` | `{code}` 兑换 CDK |
| GET | `/api/txns` | 流水（`limit`/`cursor`） |
| GET | `/api/payments` | 我的支付单 |
| POST | `/api/payments/usdt` | `{txid,chain,amount_cents}` → 待确认（`(chain,txid)` 唯一） |
| POST | `/api/payments/alipay` | 501 占位 |
| POST | `/api/alipay/notify` | 501 占位 |
| GET | `/api/plans` | 套餐列表 |
| POST | `/api/plans/buy` | `{plan_id}`；余额足够直接扣，否则生成待支付单 |
| POST | `/api/admin/cdks` | `{count,amount,batch}` 批量生成 |
| GET | `/api/admin/payments?status=pending` | 待确认列表 |
| POST | `/api/admin/payments/{id}/confirm` | 确认入账（事务 + 幂等） |

## 站点

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/sites` | `{name,base_url,webhook_url}` → `sk_…` + webhook_secret |
| GET | `/api/sites` | 我的站点（含 ai_tokens/ai_cost） |
| POST | `/api/sites/{id}/revoke` | 吊销 |
| POST | `/api/sites/{id}/rotate` | 轮换 key 与 secret |
| GET | `/api/sites/{id}/usage` | 用量 |
| GET/PUT | `/api/sites/{id}/override` | 站点级配置覆盖（`ui`/`brand`/`catalog`/`billing`） |

**站点级计费**：`PUT /api/sites/{id}/override` 的 `billing` 只接受 `task_cost` / `iterate_cost` / `credit_cost_fail`，
生成与迭代价不得低于 `billing.min_task_cost`（默认 1）；传空字符串 `{"override":""}` 表示清空、恢复平台默认。
价格解析顺序：`price_rules` → 站点 `billing` 覆盖 → 平台 `billing` 默认值。

## AI 代理（密钥只在 hub）

```
POST /api/v1/site/ai/chat      {messages:[{role,content}],mode?,session_id?}   → SSE: data: {"delta":"…"} … data: {"done":true}
POST /api/v1/site/ai/generate  {platform,mc_version,requirement}              → {task_id,cost,status}（直接入队）
```

`AI_API_KEY` 为空时进入 mock 模式，同样返回合法 SSE 与可编译的 Java 桩，便于端到端联调。

## 任务

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/v1/tasks` | 卫星站提交（`X-Site-Key` + 用户 token + 可选 `Idempotency-Key`） |
| GET | `/api/v1/tasks` | 我的任务（`limit`/`cursor`/`status`） |
| GET | `/api/v1/tasks/{id}` | 状态 + 排队位置 + `download_url`（10 分钟签名） |
| GET | `/api/v1/tasks/{id}/download` | 签名（`exp`/`sig`）或登录 + 归属校验 |
| POST | `/api/v1/tasks/{id}/iterate` | `{requirement}` 生成迭代任务（按 iterate_cost 扣费） |
| POST | `/api/v1/tasks/{id}/retry` | 失败任务的免费重试（需 `refunded=1`） |

失败策略：退 `cost - credit_cost_fail`（默认扣 1 积分覆盖 AI 成本），并允许一次免费重试。

## Worker

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/v1/worker/register` | `{name,capabilities:"paper\|spigot"}` → worker_key |
| POST | `/api/v1/worker/claim` | 原子认领（`UPDATE…RETURNING`），按 priority DESC + created_at ASC，租约 300s |
| POST | `/api/v1/worker/tasks/{id}/result` | multipart：`status=done\|failed`、`jar`、`log`、`pass_at_1`、`fix_rounds`、`ai_tokens` |
| POST | `/api/v1/worker/heartbeat` | 15–30s 一次，顺带续租 |
| GET | `/api/v1/worker/me` | 自身信息 |

## 市场

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/market/publish` | `{task_id,title,description,price}`（任务需 done） |
| GET | `/api/market` | 上架列表 |
| GET | `/api/market/{id}` | 详情 |
| POST | `/api/market/buy` | `{product_id}`，抽成 `commission_rate` 给卖家 |
| GET | `/api/market/download?product_id=` | 买家或卖家可下载 |
| GET | `/api/market/mine` | 我发布的 + 收益 |

## 订阅

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/v1/subscriptions` | `{task_id,target_versions}`，价格 `task_cost × 30/月` |
| GET | `/api/v1/subscriptions` | 列表 |
| POST | `/api/v1/subscriptions/{id}/cancel` | 取消 |
| POST | `/api/v1/subscriptions/{id}/resume` | 恢复（重置计费周期） |

后台每 5 分钟计费一次；余额不足置 `paused` 并记录原因。

## AI 中转（OpenAI 兼容）

鉴权三选一：`X-API-Key: ak_...`、`X-Site-Key: sk_...`、`Authorization: Bearer <JWT>`。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/v1/models` | 模型列表（渠道模型的并集） |
| POST | `/v1/chat/completions` | OpenAI 兼容；`stream:true` 走 SSE；按渠道优先级故障转移 |
| GET | `/v1/usage` | 自己近 7 天按天用量 |

用户侧 key：

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/ai/keys` | 我的 key（脱敏） |
| POST | `/api/v1/ai/keys` | `{name}` → 返回一次明文 `ak_...`，最多 5 个启用 |
| POST | `/api/v1/ai/keys/{id}/revoke` | 吊销 |

后台渠道：

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/admin/ai/channels` | 渠道列表（密钥打码）+ `has_env` |
| POST | `/api/admin/ai/channels` | `{name,base_url,api_key,model,priority}` |
| POST | `/api/admin/ai/channels/{id}` | 局部更新：`{name?,base_url?,api_key?,model?,priority?,enabled?}` |
| POST | `/api/admin/ai/channels/{id}/delete` | 删除 |
| GET | `/api/admin/ai/usage` | 近 7 天：调用数 / tokens / 活跃用户 / 按渠道聚合 |

行为约定：

- 渠道按 `priority DESC, id ASC` 依次尝试，上游返回 ≥400 或连接失败就换下一个，全失败返回 502。
- 上游 `api_key` 只用于服务端请求头，绝不回传给调用方；客户端拿到的只有 hub 的 `ak_`。
- 渠道 `model` 非空时覆盖客户端传入的 model；为空则透传。
- 流式请求会自动补 `stream_options.include_usage=true`，否则统计不到 tokens。
- `ai.daily_token_limit > 0` 时按用户当日累计 tokens 限流，超限 429。

## 后台

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/admin/dashboard` | 用户/任务/队列/成功率/积分发放/待确认支付/worker 在线 |
| GET | `/api/admin/users` | 用户列表 |
| GET | `/api/admin/sites` | 站点列表（含 AI 消耗） |
| GET | `/api/admin/workers` | worker 列表与在线状态 |
| GET | `/api/admin/audit` | 审计日志（分页） |
| POST | `/api/admin/price-rules` | `{action,site_id,platform,amount,currency,priority}` |

## 运维

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/healthz` | 存活 + config_version |
| GET | `/api/version` | 版本信息 |

## 事件（outbox）

`task.created` / `task.done` / `task.failed` 会以 HMAC 签名回调站点 webhook：

```
POST <webhook_url>
X-MCAI-Event: task.done
X-MCAI-Timestamp: 1730000000
X-MCAI-Signature: hex(HMAC-SHA256(webhook_secret, "<ts>." + body))
```

失败按 10s → 20s → 40s 重试 3 次，仍失败进死信（`events.status=3`）。
