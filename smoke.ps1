#!/usr/bin/env pwsh
# MC AI 主站端到端冒烟：全部走 mock AI，不依赖外部服务。
# 用法: pwsh -File .\smoke.ps1 [-Base http://127.0.0.1:8080] [-AdminPassword admin123]

param(
  [string]$Base = "http://127.0.0.1:8080",
  [string]$AdminPassword = "admin123"
)

$ErrorActionPreference = "Stop"
$script:fail = 0
$script:pass = 0

function Section([string]$t) { Write-Host "`n== $t ==" -ForegroundColor Cyan }
function Check([bool]$cond, [string]$msg) {
  if ($cond) { $script:pass++; Write-Host "  PASS $msg" -ForegroundColor Green }
  else { $script:fail++; Write-Host "  FAIL $msg" -ForegroundColor Red }
}

function PostJson([string]$path, $body, $headers) {
  $h = @{ "Content-Type" = "application/json" }
  if ($headers) { foreach ($k in $headers.Keys) { $h[$k] = $headers[$k] } }
  $json = if ($body -is [string]) { $body } else { $body | ConvertTo-Json -Depth 10 -Compress }
  Invoke-RestMethod -Method Post -Uri "$Base$path" -Headers $h -Body $json
}

function GetJson([string]$path, $headers) {
  $h = @{}
  if ($headers) { foreach ($k in $headers.Keys) { $h[$k] = $headers[$k] } }
  Invoke-RestMethod -Method Get -Uri "$Base$path" -Headers $h
}

function TryReq([scriptblock]$fn) {
  try { & $fn } catch { $null }
}

# ---------------------------------------------------------------- 0. 健康
Section "0 健康检查"
try {
  $h = GetJson "/healthz"
  Check ($h.ok -eq $true) "GET /healthz 返回 ok"
} catch {
  Write-Host "  主站没起来：$Base —— 先运行 go run ./cmd/hub" -ForegroundColor Red
  exit 1
}

# ---------------------------------------------------------------- 1. 注册
Section "1 注册（余额应为 0）"
$suffix = [Guid]::NewGuid().ToString("N").Substring(0, 8)
$email = "u_$suffix@example.com"
$password = "Passw0rd!123"
$reg = PostJson "/api/register" @{ email = $email; username = "u_$suffix"; password = $password }
Check ($reg.token.Length -gt 20) "注册返回 token"
Check ($reg.user.balance -eq 0) "新账号余额为 0（赠送延后到首单成功）"
$uh = @{ Authorization = "Bearer $($reg.token)" }

$me = GetJson "/api/me" $uh
Check ($me.email -eq $email) "GET /api/me 返回本人"

# ---------------------------------------------------------------- 2. 管理员
Section "2 管理员登录 + CDK"
$adm = PostJson "/api/login" @{ identifier = "admin"; password = $AdminPassword }
Check ($adm.user.role -eq "admin") "admin 登录成功"
$ah = @{ Authorization = "Bearer $($adm.token)" }

$cdk = PostJson "/api/admin/cdks" @{ count = 2; amount = 100; batch = "smoke" } $ah
Check ($cdk.codes.Count -eq 2) "生成 2 张 CDK"
$code1 = $cdk.codes[0]
$code2 = $cdk.codes[1]

$rd = PostJson "/api/redeem" @{ code = $code1 } $uh
Check ($rd.balance -eq 100) "兑换 CDK 后余额 100"

# ---------------------------------------------------------------- 3. 配置
Section "3 公开配置 + 站点 + 站点配置下发"
$pub = GetJson "/api/config"
Check ($null -ne $pub.billing.task_cost) "GET /api/config 含 billing.task_cost"
Check ($pub.config_version -gt 0) "config_version 存在"

$st = PostJson "/api/sites" @{ name = "smoke-site"; base_url = "https://smoke.example.com" } $uh
Check ($st.site_key -like "sk_*") "创建站点返回 sk_ key"
$sh = @{ "X-Site-Key" = $st.site_key }

$sc = GetJson "/api/v1/site/config" $sh
Check ($sc.config_version -gt 0) "站点配置带 config_version"
Check ($null -ne $sc.catalog.mc_versions) "站点配置含版本矩阵"
Check ($null -eq $sc.ai_api_key) "站点配置不含 AI 密钥"

$etag = (Invoke-WebRequest -Uri "$Base/api/v1/site/config" -Headers $sh -SkipHttpErrorCheck)
$e = $etag.Headers["ETag"]
if ($e -is [array]) { $e = $e[0] }
$r304 = Invoke-WebRequest -Uri "$Base/api/v1/site/config" -Headers (@{ "X-Site-Key" = $st.site_key; "If-None-Match" = $e }) -SkipHttpErrorCheck
Check ($r304.StatusCode -eq 304) "ETag 协商返回 304"

# ---------------------------------------------------------------- 4. AI 对话
Section "4 AI 代理（mock 流式）"
$chatBody = @{ mode = "restate"; messages = @(@{ role = "user"; content = "做一个抽奖插件" }) } | ConvertTo-Json -Depth 6 -Compress
$sse = Invoke-WebRequest -Method Post -Uri "$Base/api/v1/site/ai/chat" `
  -Headers (@{ "X-Site-Key" = $st.site_key; "Authorization" = "Bearer $($reg.token)"; "Content-Type" = "application/json" }) `
  -Body $chatBody
Check ($sse.Content -match "data:") "SSE 返回 data: 事件"
Check ($sse.Content -match "done") "SSE 以 done 结束"

# ---------------------------------------------------------------- 5. 生成任务
Section "5 生成任务（扣 10 积分）"
$gen = PostJson "/api/v1/site/ai/generate" @{
  platform = "paper"; mc_version = "1.20.4"; requirement = "抽奖插件：/lottery 打开奖池，冷却 60 秒"
} (@{ "X-Site-Key" = $st.site_key; "Authorization" = "Bearer $($reg.token)" })
Check ($gen.task_id -gt 0) "生成返回 task_id"
Check ($gen.cost -eq 10) "计价 10 积分"

$taskId = $gen.task_id
$afterGen = GetJson "/api/me" $uh
Check ($afterGen.balance -eq 90) "生成后余额 90"

# 幂等：同 key 再提交一次不应重复扣费
$idemKey = [Guid]::NewGuid().ToString()
$g1 = PostJson "/api/v1/site/ai/generate" @{ platform = "paper"; mc_version = "1.20.4"; requirement = "幂等测试" } (@{
  "X-Site-Key" = $st.site_key; "Authorization" = "Bearer $($reg.token)"; "Idempotency-Key" = $idemKey })
$g2 = PostJson "/api/v1/site/ai/generate" @{ platform = "paper"; mc_version = "1.20.4"; requirement = "幂等测试" } (@{
  "X-Site-Key" = $st.site_key; "Authorization" = "Bearer $($reg.token)"; "Idempotency-Key" = $idemKey })
Check ($g1.task_id -eq $g2.task_id) "相同 Idempotency-Key 返回同一任务"
$afterIdem = GetJson "/api/me" $uh
Check ($afterIdem.balance -eq 80) "幂等提交只扣一次（余额 80）"

# ---------------------------------------------------------------- 6. worker
Section "6 worker 注册 / 认领 / 上报"
$wk = PostJson "/api/v1/worker/register" @{ name = "smoke-worker"; capabilities = "paper|spigot" }
Check ($wk.worker_key.Length -gt 10) "worker 注册成功"
$wh = @{ "X-Worker-Key" = $wk.worker_key }

$claim = TryReq { PostJson "/api/v1/worker/claim" @{} $wh }
Check ($null -ne $claim -and $claim.task_id -gt 0) "worker 认领到任务"

if ($null -ne $claim -and $claim.task_id -gt 0) {
  $jar = Join-Path $env:TEMP ("smoke-{0}.jar" -f $claim.task_id)
  [IO.File]::WriteAllBytes($jar, [byte[]](0..63))
  $res = Invoke-RestMethod -Method Post -Uri "$Base/api/v1/worker/tasks/$($claim.task_id)/result" -Headers $wh -Form @{
    status = "done"; pass_at_1 = "1"; fix_rounds = "0"; ai_tokens = "1234"; jar = Get-Item $jar
  }
  Check ($res.status -eq "done") "worker 上报完成"

  $t = GetJson "/api/v1/tasks/$($claim.task_id)" $uh
  Check ($t.status -eq "done") "任务状态 done"
  Check ($t.pass_at_1 -eq 1) "pass_at_1 已记录"
  Check ($null -ne $t.download_url) "返回签名下载地址"

  $dl = Invoke-WebRequest -Uri "$Base$($t.download_url)" -SkipHttpErrorCheck
  Check ($dl.StatusCode -eq 200 -and $dl.RawContentLength -gt 0) "签名地址可下载产物"

  $meAfter = GetJson "/api/me" $uh
  Check ($meAfter.signup_granted -eq 1) "首单成功后标记已发放"
  Check ($meAfter.balance -eq 100) "首单赠送 +20（余额 100）"
}

# ---------------------------------------------------------------- 7. 市场
Section "7 市场发布 / 购买"
$prod = PostJson "/api/market/publish" @{ task_id = $taskId; title = "抽奖插件"; description = "smoke"; price = 0 } $uh
Check ($prod.product_id -gt 0) "发布商品"

$suffix2 = [Guid]::NewGuid().ToString("N").Substring(0, 8)
$buyer = PostJson "/api/register" @{ email = "b_$suffix2@example.com"; username = "b_$suffix2"; password = $password }
$bh = @{ Authorization = "Bearer $($buyer.token)" }
$order = PostJson "/api/market/buy" @{ product_id = $prod.product_id } $bh
Check ($order.ok -eq $true) "购买成功"
$dl2 = Invoke-WebRequest -Uri "$Base/api/market/download?product_id=$($prod.product_id)" -Headers $bh -SkipHttpErrorCheck
Check ($dl2.StatusCode -eq 200) "买家可下载"

# ---------------------------------------------------------------- 8. 订阅
Section "8 订阅"
$sub = PostJson "/api/v1/subscriptions" @{ task_id = $taskId; target_versions = @("1.21.1") } $uh
Check ($sub.id -gt 0) "创建订阅"
$subs = GetJson "/api/v1/subscriptions" $uh
Check ($subs.items.Count -ge 1) "订阅列表返回"

# ---------------------------------------------------------------- 9. 后台
Section "9 后台统计 / 审计"
$dash = GetJson "/api/admin/dashboard" $ah
Check ($dash.users -ge 2) "dashboard 用户数正确"
Check ($dash.done -ge 1) "dashboard 完成任务数 ≥ 1"
Check ($dash.config_version -gt 0) "dashboard 带配置版本"

$audit = GetJson "/api/admin/audit" $ah
Check ($audit.items.Count -ge 1) "审计日志有记录"

# 配置热更新 → 新版本自动生成适配任务
$before = GetJson "/api/admin/dashboard" $ah
$cfg = PostJson "/api/admin/config" @{ section = "catalog"; values = @{ mc_versions = @("1.8.8","1.12.2","1.16.5","1.18.2","1.20.4","1.21.1","1.21.3") } } $ah
Check ($cfg.config_version -gt $pub.config_version) "改配置后版本号递增"
Check ($cfg.added_versions.Count -eq 1) "识别出新增版本 1 个"
Check ($cfg.adapt_tasks -ge 1) "为订阅自动生成适配任务"

# ---------------------------------------------------------------- 结果
Write-Host "`n========================================" -ForegroundColor Cyan
Write-Host ("通过 {0} 项，失败 {1} 项" -f $script:pass, $script:fail) -ForegroundColor $(if ($script:fail -eq 0) { "Green" } else { "Red" })
if ($script:fail -gt 0) { exit 1 }
