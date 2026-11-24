# 源码归属

初始导入的 269 个 Go 文件来自 `kiss-kedaya/sub2api` 提交 `4902f6d7b7264fdeb667d8aca5c170dc38c61f11` 的 `backend/internal/cryptopay/`。上游声明版权仍归原 Sub2API 作者，采用 GNU LGPL v3.0 或更新版本。[固定源码](https://github.com/kiss-kedaya/sub2api/tree/4902f6d7b7264fdeb667d8aca5c170dc38c61f11/backend/internal/cryptopay) · [上游许可声明](https://github.com/kiss-kedaya/sub2api/blob/4902f6d7b7264fdeb667d8aca5c170dc38c61f11/README_CN.md#许可证) · [许可正文](../../../LICENSE)

首次导入提交为 `eb01f01eb229bd68583c8902a95605783156dd4b`。269 个 Go 文件中，116 个与归档逐字节相同，153 个只替换 Go 模块前缀。可在应用仓库用 `git grep -l 'github.com/TokenFlux/TokenRouter/' eb01f01e -- ':(glob)backend/internal/cryptopay/**/*.go'` 查询该导入版本，并用 `git diff eb01f01e HEAD -- backend/internal/cryptopay` 复核后续宿主适配。

固定归档 `source.tar.gz` 的 PAX `comment` 为上述提交号，SHA-256 为 `18e6513cea4e80b497459fb1718665cafe9faa7a7f3aa4d09fc21aa52d6bdb15`。

同一固定版本还提供了 `backend/internal/handler/crypto*.go`、支付订单冲正、支付管理员授权、Bark 通知、收款迁移、用户充值页和后台钱包界面。目标仓库将这些代码接入 `internal/payment`、`internal/identity`、`internal/notification`、本目录的 `httpapi` 和 `frontend/src`。[源端运行时装配](https://github.com/kiss-kedaya/sub2api/blob/4902f6d7b7264fdeb667d8aca5c170dc38c61f11/backend/cmd/server/payment_runtime.go) · [源端路由](https://github.com/kiss-kedaya/sub2api/blob/4902f6d7b7264fdeb667d8aca5c170dc38c61f11/backend/internal/server/routes/crypto.go)
