<div align="center">
  <p>
    <img src="assets/readme/banner-zh.svg" width="100%" alt="TokenRouter，开源的大模型统一网关。Messages、Responses、Chat Completions 和 Gemini 请求从同一个入口进入，转发到 Anthropic、OpenAI、Gemini 等上游平台，上游出错时自动切换。" />
  </p>

  <p>
    <a href="https://github.com/y1yyum/tokenrouter-custom-public/actions/workflows/ci.yml"><img src="https://img.shields.io/github/actions/workflow/status/y1yyum/tokenrouter-custom-public/ci.yml?branch=main&style=flat-square&label=CI&logo=githubactions&logoColor=white&labelColor=27272A" alt="CI" /></a>
    <a href="https://github.com/y1yyum/tokenrouter-custom-public/actions/workflows/security-scan.yml"><img src="https://img.shields.io/github/actions/workflow/status/y1yyum/tokenrouter-custom-public/security-scan.yml?branch=main&style=flat-square&label=Security&logo=githubactions&logoColor=white&labelColor=27272A" alt="Security Scan" /></a>
    <a href="https://github.com/TokenFlux/TokenRouter/commit/4ced45bb1854eb3296015fb4b092b64a8c263b46"><img src="https://img.shields.io/badge/upstream-TokenFlux%2FTokenRouter%20%40%204ced45bb-3F3F46?style=flat-square&labelColor=27272A" alt="上游基线: TokenFlux/TokenRouter @ 4ced45bb" /></a>
    <a href="LICENSE"><img src="https://img.shields.io/badge/license-LGPL--3.0--or--later-3F3F46?style=flat-square&labelColor=27272A" alt="License: LGPL-3.0-or-later" /></a>
  </p>

  <p><strong>简体中文</strong> · <a href="README.md">English</a> · <a href="#定制增强">定制增强</a> · <a href="#配套插件">配套插件</a> · <a href="#快速开始">快速开始</a> · <a href="#文档">文档</a></p>
</div>

## 简介

`y1yyum/tokenrouter-custom-public` 是 [TokenFlux/TokenRouter](https://github.com/TokenFlux/TokenRouter) 的公开定制分支，已同步至官方提交 `4ced45bb`（`v0.7.6` 之后）。TokenRouter 将 Anthropic、OpenAI、Gemini 等多家厂商的模型服务聚合到统一网关入口，提供协议转换、调度故障转移、计费额度控制以及 Vue 3 管理后台。

在官方上游能力的基础上，本仓库补充了按模型粒度的定时质量检测与隔离、只读路由诊断与排除原因预览、24 小时调度质量指标、HTTP SSE 与 WebSocket Responses 流式单次结算、门户与模型广场体验修复，以及站点管理 Key 的生命周期权限收口。本仓库仅包含源码、数据库迁移、测试与部署模板，不含任何生产凭据或私有环境配置。

> [!WARNING]
> 本仓库为独立维护的二次开发分支，不由 TokenFlux 官方运营、授权或背书。在遵守适用开源许可证的前提下，本项目可用于搭建商业 API 中转服务，但不得冒充 TokenFlux 官方站点（[tokenflux.com](https://tokenflux.com)）或造成官方关联混淆。建议商业运营者定制前端并清晰标明实际运营主体，详见[商业使用与品牌声明](BRANDING.md)。

## 定制增强

### 定时质量检测与单模型隔离

管理员可在定时探测计划（`scheduled_test_plans`）上按需启用带评分的 Responses 质量检测（`quality_enabled`，见迁移 `318_scheduled_quality.sql`）。服务端校验固定的算术 JSON 整数数组响应（`arithmetic-array-v1`），将 `pass`、`fail` 与传输异常等 `ungraded` 状态分开入账。

有效评分按 `(provider_id, upstream_model, plan_id)` 带时间水位防重放地写入 `provider_plan_model_verdicts` 与 `provider_plan_model_state`（见迁移 `317_provider_plan_model_verdicts.sql`）。检测答错仅隔离该提供商上的对应上游模型，计划暂停后最长保留 24 小时；同一提供商上的其他模型及其他提供商上的同名模型仍可继续调度，且只有产生该限制的计划答对后才负责解除。

在配置文件开启 `gateway.openai.plan_model_restrictions`（`deploy/config.example.yaml` 中默认 `false`）后，网关会在候选筛选与每次发出前重新读取已提交判定，覆盖以下入口：

- OpenAI Responses HTTP（`POST /v1/responses`）
- OpenAI Responses WebSocket（`GET /v1/responses`），含 `per_session`、`pooled` 与 `http_bridge` 三种模式
- Compact 专用端点（`/v1/responses/compact`）与请求体触发的原生压缩
- Chat Completions（`/v1/chat/completions`）与 Anthropic Messages（`/v1/messages`）跨组回退至 Responses 后的目的组

已绑定 `previous_response_id` 的 continuation 请求在发送前命中限制时直接终止，不静默切换至其他提供商。

### 模型排除原因、路由预览与调度指标

管理员可在「属性管理 → 质量检测」以及 `/admin/model-exclusions` 页面查看计划状态、限制归属与调度资格。只读的 HTTP 路由预览展示客户端入口、来源组、目的组、协议转换路径、最终上游模型以及具体排除原因，且不会发出上游请求或产生用量记录。

质量视图按 `(provider_id, upstream_model)` 统计 UTC 最近 24 小时的四项指标，分母为零时显示 `Unknown`：

| 指标 | 计算口径 |
| --- | --- |
| 观测可用率 | 用户成功请求数除以用户成功请求数与可归因终态提供商失败数之和 |
| 缓存请求命中率 | 包含缓存读取的成功请求数除以输入量大于零的成功请求数 |
| 缓存 token 占比 | 缓存读取 token 数除以普通输入、缓存写入与缓存读取 token 总和 |
| 合成质检失败率 | 有效评分失败次数除以有效评分总数（不包含 `ungraded` 传输失败） |

### 流式用量核对与缓存分档结算

HTTP SSE 与 WebSocket Responses 均保留终帧前已回报的输入、缓存与输出用量；Messages 的 5 分钟/1 小时与 Responses 的 30 分钟/12 小时缓存写入字段按对应分档计价。在用量晚到、完成帧重复发送以及客户端断流重试场景下，每轮请求仅落库一条用量记录并生成唯一的资金去重键。

### 门户交互、模型广场与外链隔离

- 自定义嵌入页（`CustomPageView`）在跳转外部链接时不携带宿主登录态与凭据，返回宿主后可正常刷新用户信息。
- 模型广场将空目录与搜索无匹配明确区分，保留游客搜索词并在登录后回到原搜索结果，切换登录身份时清除失效的品牌筛选，并统一游客公开分组与登录后专属分组的报价口径。
- 普通用户的 `/dashboard` 概览与 `/usage` 用量明细统一按浏览器 IANA 时区切分小时、日期与热力图，切换模型筛选时立即重置分页并清除旧总数，列表请求失败时隐藏旧数据并提供原地重试。

### 管理权限与站点 Key 边界

站点管理 Key（`x-api-key`）保留对常规兼容管理接口的查询访问权限，但查看其脱敏状态、轮换或删除自身时必须使用管理员 JWT 会话（轮换响应附带 `Cache-Control: no-store`）。`/mcp` 接口按操作 ID 授权集合、读写属性、浏览器确认策略及团队 owner/member 角色执行校验。

## 配套插件

上游站点余额监控与联动控制由独立仓库 [y1yyum/tokenrouter-upstream-sites](https://github.com/y1yyum/tokenrouter-upstream-sites)（插件 ID `local.upstream-sites`，已在当前 `65cda151` 基线验收 `v1.1.1`）提供。该插件通过官方插件 SDK 运行，在后台 `/admin/plugins` 上传安装包即可使用，无需修改主程序代码。

插件支持多站点账号余额查询、可选余额倍率与人民币成本估算，并可按需在 PostgreSQL 安装 `control_bridge.sql`，在低余额时自动停调已绑定的提供商，并在余额恢复后保留管理员原有或期间设置的人工停调状态。

## 基础能力与协议入口

作为 TokenRouter 的二开分支，主程序沿用原生路由、上游适配、计费与后台管理能力。各平台支持的详细端点见[上游提供商能力矩阵](docs/interfaces/upstream_provider_matrix.md)。

| 方向 | 说明 |
| --- | --- |
| 协议与入口 | Anthropic Messages（`POST /v1/messages`）、OpenAI Responses / Chat（`POST /v1/responses`、`POST /v1/chat/completions`）、Gemini（`POST /v1beta/models/{model}:generateContent`、`:streamGenerateContent`）、WebSocket（`GET /v1/responses`、`GET /v1/realtime`）、向量（`POST /v1/embeddings`）、图片（`/v1/images/generations`、`/edits`、`/batches`）、视频（`/v1/videos/generations`、`/edits`、`/extensions`）、语音（`/v1/tts`、`/v1/stt`、`/v1/custom-voices`）、搜索（`/v1/web_search`、`/v1/x_search`、`/v1/alpha/search`）、模型与用量（`GET /v1/models`、`GET /v1/usage`） |
| 上游平台 | Anthropic、OpenAI、Gemini、Antigravity、Grok、Qoder、Kimi、智谱、DeepSeek、OpenCode、Jev，支持 OAuth、Setup Token、API Key、AWS Bedrock、Vertex AI 与 Cosy |
| 访问控制 | 用户、团队、分组、API Key 额度与并发/RPM 限制；支持邮箱密码、Passkey、TOTP 两步验证以及 GitHub、Google、LinuxDo、OIDC、微信与钉钉登录 |
| 计费与支付 | 按用量计费，支持余额、订阅套餐、额度包、兑换码与推广返利；内置易支付、支付宝、微信支付、Stripe、Airwallex 与 EPUSDT（EPay 协议） |
| 运维与扩展 | 关键词与哈希内容审核、用量日志与后台异步 CSV 导出、网页创作台、在线更新回退，以及 MIT 许可的 Go/Web 插件 SDK |

## 界面预览

### 仪表盘

<img src="assets/screenshots/dashboard-dark.jpg" alt="TokenRouter 仪表盘 · 深色模式" width="1600" />

### 模型广场

<img src="assets/screenshots/models-dark.jpg" alt="TokenRouter 模型广场 · 深色模式" width="1600" />

<details>
<summary>浅色模式截图</summary>

### 仪表盘

<img src="assets/screenshots/dashboard-light.jpg" alt="TokenRouter 仪表盘 · 浅色模式" width="1600" />

### 模型广场

<img src="assets/screenshots/models-light.jpg" alt="TokenRouter 模型广场 · 浅色模式" width="1600" />

</details>

## 快速开始

> [!NOTE]
> `deploy/docker-deploy.sh` 与 `deploy/install.sh` 指向官方 `TokenFlux/TokenRouter` 发行资产。部署包含本分支定制功能的实例时，请基于 `y1yyum/tokenrouter-custom-public` 源码构建运行。

### 方式一：使用 Docker Compose 从源码构建启动

克隆仓库并启动 TokenRouter、PostgreSQL 18 与 Redis 8：

```bash
git clone https://github.com/y1yyum/tokenrouter-custom-public.git
cd tokenrouter-custom-public/deploy

cp .env.example .env
chmod 600 .env
# 编辑 .env，设置强密码 POSTGRES_PASSWORD、JWT_SECRET 与 TOTP_ENCRYPTION_KEY

docker compose -f docker-compose.dev.yml up --build -d
```

若要配合 `deploy/docker-compose.local.yml` 部署本地编译镜像，可先在仓库根目录执行构建脚本，并将 `docker-compose.local.yml` 中的 `pull_policy: always` 改为 `never`（或改用自有镜像标签），避免 Compose 启动时拉取上游同名镜像覆盖本地构建：

```bash
./deploy/build_image.sh
```

服务启动后访问 `http://127.0.0.1:8080`。若 `.env` 未设置 `ADMIN_PASSWORD`，系统会在首次启动日志中输出随机生成的管理员密码：

```bash
docker compose -f deploy/docker-compose.dev.yml logs tokenrouter | grep "admin password"
```

### 方式二：开启定时质量拦截

新部署默认关闭质量拦截以保证路由兼容性。按需启用步骤如下：

1. 在持久化配置 `config.yaml` 中设置 `gateway.openai.plan_model_restrictions: true` 并重启服务。
2. 在后台提供商的定时检测计划中为目标计划开启 `quality_enabled`。

## 数据库迁移与升级约束

本分支在 `backend/migrations/` 下包含 354 份前向 SQL 迁移脚本（在上游 `001` 至 `316` 基础上新增 `317_provider_plan_model_verdicts.sql` 与 `318_scheduled_quality.sql`），随服务端二进制内嵌并在启动时自动执行。

升级或回退注意事项：

- 升级前先停止外部新流量并优雅停机，将后端与前端产物作为同一版本整体替换。
- 迁移前务必完整备份 PostgreSQL、Redis、`config.yaml`（v2 格式）以及 `/app/data`（包含 `plugins/`、`pages/` 与安装锁）。
- 将 `gateway.openai.plan_model_restrictions` 设为 `false` 即可关闭质量拦截，无需改动数据库表结构；若需撤销已执行的数据库迁移，必须恢复升级前备份。
- 此前使用站点 `x-api-key` 查询、轮换或删除该密钥自身的自动化客户端会收到 HTTP 403，需切换为管理员 JWT 会话。

更多步骤见[部署与数据库迁移](docs/operations/deployment_and_migrations.md)和[版本升级说明](docs/operations/upgrade_notes.md)。

## 本地开发与验证

仓库锁定的基础工具链版本：

| 组件 | 版本来源 | 当前要求 |
| --- | --- | --- |
| Go | `backend/go.mod` | `1.27.4` |
| Node.js | `.node-version` | `26.12.1` |
| pnpm | `frontend/package.json` | `12.9.1` |
| golangci-lint | `.golangci-version` | `v2.14.0` |
| PostgreSQL / Redis | `deploy/docker-compose.local.yml` | `postgres:18-alpine`、`redis:8-alpine` |

常用 `Makefile` 目标：

```bash
# 安装本地 pre-push 快检钩子
make hooks

# 静态检查、单元测试与内嵌前端整站构建
make lint
make test
make build
make test-embed

# 集成测试（需要 Docker）与工程文档一致性检查
make test-integration
make check-docs

# 完整本地 CI 等价检查与依赖安全扫描
make verify
make security
```

每次推送与 Pull Request 由 `.github/workflows/ci.yml`（`plugin-sdk`、`go-lint`、`go-test`、`go-integration`、`frontend`、`build`、`scripts`、`installer`）及 `.github/workflows/security-scan.yml`（`backend-security`、`frontend-security`）在 Ubuntu 环境执行验证。

## 文档

- [工程文档总目录](docs/index.md)：系统架构、领域不变量、接口契约与运维约束。
- [使用与运维指南](docs/guides/index.md)：部署、支付配置与插件开发手册。
- [开发、验证与上游同步](docs/operations/development_workflow.md)：依赖门禁、后端文件布局、测试分层与分支同步规则。
- [Docker 镜像说明](deploy/DOCKER.md)：Compose 部署变体、独立容器环境变量与 v2 配置升级说明。

## 许可证与安全

本项目依据 [GNU Lesser General Public License v3.0 或更高版本](LICENSE)发布，`plugin-sdk/` 目录使用 [MIT 许可证](plugin-sdk/LICENSE)。

Copyright (c) 2026 Wesley Liddick & TokenFlux，包含本仓库贡献者的二次开发修改。

如发现影响本分支的安全问题，请通过本仓库的 [GitHub Private Vulnerability Reporting](https://github.com/y1yyum/tokenrouter-custom-public/security/advisories) 私密提交，切勿直接在公开 Issue 披露。未修改上游代码的共性漏洞报告规范见 [SECURITY.md](SECURITY.md)。
