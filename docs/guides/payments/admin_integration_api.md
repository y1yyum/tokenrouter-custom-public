# 外部支付管理 API 集成

本文档用于对接外部支付系统（如 `sub2apipay`）与 TokenRouter 的管理 API。其他操作手册见 [指南目录](../index.md)，接口规则见 [HTTP 接口 Project Doc](../../interfaces/http_api.md)。

## 目标

本文覆盖：

- 支付成功后充值
- 用户查询
- 人工余额修正
- 自定义菜单中外部支付页的展示参数

## 基础地址

- 生产：`https://<your-domain>`

## 认证

推荐使用以下请求头：

- `x-api-key: admin-<64hex>`
- `Content-Type: application/json`
- 幂等接口额外传入 `Idempotency-Key`

管理员 JWT 也可访问管理路由，但服务间调用应使用管理 API Key。

## 一步完成创建并兑换

`POST /api/v1/admin/redeem-codes/create-and-redeem`

这个接口原子地完成"创建兑换码，并兑换给指定用户"。

请求头：

- `x-api-key`
- `Idempotency-Key`

请求体示例：

```json
{
  "code": "s2p_cm1234567890",
  "type": "balance",
  "value": 100.0,
  "user_id": 123,
  "notes": "sub2apipay order: cm1234567890"
}
```

幂等规则：

- 同一 `code` 且 `used_by` 一致：返回 `200`
- 同一 `code` 但 `used_by` 不一致：返回 `409`
- 缺少 `Idempotency-Key`：返回 `400`（`IDEMPOTENCY_KEY_REQUIRED`）

调用示例：

```bash
curl -X POST "${BASE}/api/v1/admin/redeem-codes/create-and-redeem" \
  -H "x-api-key: ${KEY}" \
  -H "Idempotency-Key: pay-cm1234567890-success" \
  -H "Content-Type: application/json" \
  -d '{
    "code":"s2p_cm1234567890",
    "type":"balance",
    "value":100.00,
    "user_id":123,
    "notes":"sub2apipay order: cm1234567890"
  }'
```

## 查询用户（可选前置校验）

`GET /api/v1/admin/users/:id`

```bash
curl -s "${BASE}/api/v1/admin/users/123" \
  -H "x-api-key: ${KEY}"
```

## 余额调整

`POST /api/v1/admin/users/:id/balance`

该接口用于人工补偿或扣减，支持 `set`、`add` 和 `subtract`。

请求体示例（扣减）：

```json
{
  "balance": 100.0,
  "operation": "subtract",
  "notes": "manual correction"
}
```

```bash
curl -X POST "${BASE}/api/v1/admin/users/123/balance" \
  -H "x-api-key: ${KEY}" \
  -H "Idempotency-Key: balance-subtract-cm1234567890" \
  -H "Content-Type: application/json" \
  -d '{
    "balance":100.00,
    "operation":"subtract",
    "notes":"manual correction"
  }'
```

## 自定义菜单的展示参数

原生充值和订阅入口为 `/purchase`。通过自定义菜单接入的外部支付页使用 `/custom/:id`。旧充值设置迁移生成的菜单 ID 为 `migrated_purchase_subscription`。

自定义菜单的 iframe 和“新标签页打开”使用同一个 HTTP(S) 地址。宿主设置 `theme`（`light` 或 `dark`）、`lang`（当前界面语言）和 `ui_mode=embedded`。切换主题或语言时，两个入口同步更新。配置中的套餐参数和片段路由均保留，例如

```text
https://pay.example.com/pay?plan=pro&theme=dark&lang=zh&ui_mode=embedded#checkout
```

宿主从菜单地址移除旧参数 `user_id`、`token`、`src_host` 和 `src_url`，并在打开页面时省略 HTTP `Referer`。外部支付系统需自行确认订单对应的 TokenRouter 账户，再由服务端调用管理接口。

## 失败处理建议

- 分别持久化支付成功和充值成功状态。
- 回调验签成功后立即标记支付成功。
- 支付成功但充值失败的订单应允许后续重试。
- 同一笔充值因超时或网络错误重试时，保持相同 `code`、请求体和 `Idempotency-Key`。
- 只有发起一笔内容不同的新操作时，才生成新的 `Idempotency-Key`。

## `doc_url` 配置建议

- 查看链接：`https://github.com/TokenFlux/TokenRouter/blob/main/docs/guides/payments/admin_integration_api.md`
- 下载链接：`https://raw.githubusercontent.com/TokenFlux/TokenRouter/main/docs/guides/payments/admin_integration_api.md`
