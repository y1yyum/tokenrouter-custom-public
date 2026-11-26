// cryptoAccount.ts supplies account messages for key rotation and notifications.
// Account labels shared by the deposit gate and the Bark settings card.
export default {
  keyRotationGate: {
    title: '充值 / 兑换暂不可用',
    description: '为保护您的账户安全，请先更换 API Key 或为其设置 IP 白名单后再充值/兑换。',
    activeKeysLabel: '启用中的 Key',
    pendingKeysLabel: '待整改 Key',
    hint: '更换一个新的 API Key，或为每个启用中的 Key 配置 IP 白名单，完成后本页自动恢复。模型调用、查看余额和密钥管理不受影响。',
    action: '前往 API 密钥',
    riskAckAction: '我已知晓风险，继续充值',
    riskAckConfirmTitle: '确认跳过整改？',
    riskAckConfirmMessage:
      '不轮换 API Key、不设置 IP 白名单时，一旦 Key 泄漏，他人可直接盗刷您的余额。风险由您自行承担，确认继续充值 / 兑换？',
    riskAckConfirm: '确认继续',
    riskAckCancel: '取消',
    riskAckLoading: '处理中...',
    riskAckFailed: '风险确认失败，请稍后重试，或先完成 API Key 整改。',
    riskAckExpiry: '风险确认有效期至 {time}',
    riskAckBanner: '建议尽快轮换 API Key 或设置 IP 白名单，到期后需要重新确认。',
  },
  bark: {
    title: 'Bark 推送通知',
    description: '通过 Bark 在 iPhone 上接收到账通知',
    deviceKey: 'Bark 设备 Key',
    deviceKeyPlaceholder: '在 Bark App 首页复制，形如 xxxxxxxx',
    deviceKeyHint: 'Key 会加密存储，保存后不再显示明文',
    keepKeyHint:
      '已保存的 Key 不会回显。留空保存仅更新推送开关；填写新 Key 才会替换，清除请使用“清除”按钮。',
    loadFailed: 'Bark 配置读取失败，刷新成功后才能修改。',
    changeVerificationHint: '保存变更前会通知原 Bark 设备。如果通知发送失败，原配置继续生效。',
    configured: '已配置',
    notConfigured: '未配置',
    enabled: '启用推送',
    save: '保存',
    clear: '清除',
    saved: '已保存',
    cleared: '已清除',
  },
}
