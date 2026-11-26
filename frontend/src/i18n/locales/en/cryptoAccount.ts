// Account labels shared by the deposit gate and the Bark settings card.
export default {
  keyRotationGate: {
    title: 'Recharge and redemption are paused',
    description:
      'To protect your account, rotate your API key or set an IP allowlist for it before you recharge or redeem a code.',
    activeKeysLabel: 'Active keys',
    pendingKeysLabel: 'Keys to fix',
    hint: 'Rotate an API key, or add an IP allowlist to every active key, and this page recovers automatically. Model calls, balance reads and key management stay available.',
    action: 'Go to API Keys',
    riskAckAction: 'I understand the risk, continue recharging',
    riskAckConfirmTitle: 'Skip the remediation?',
    riskAckConfirmMessage:
      'If you neither rotate your API keys nor set an IP allowlist, anyone who obtains a leaked key can spend your balance directly. You accept that risk yourself. Continue recharging / redeeming?',
    riskAckConfirm: 'Continue anyway',
    riskAckCancel: 'Cancel',
    riskAckLoading: 'Working...',
    riskAckFailed:
      'Could not record the risk acknowledgement. Try again later or fix your API keys first.',
    riskAckExpiry: 'Risk acknowledgement valid until {time}',
    riskAckBanner:
      'Rotate your API keys or set IP allowlists as soon as possible; you will have to acknowledge again after it expires.',
  },
  bark: {
    title: 'Bark Push Notifications',
    description: 'Receive deposit notifications on your iPhone via Bark',
    deviceKey: 'Bark device key',
    deviceKeyPlaceholder: 'Copy from the Bark app home screen, e.g. xxxxxxxx',
    deviceKeyHint: 'The key is stored encrypted and never shown again after saving',
    keepKeyHint:
      'The saved key is never shown. Leave this field blank to update only the toggle, or enter a new key to replace it. Use Clear to remove it.',
    loadFailed: 'Bark settings could not be loaded. Refresh successfully before making changes.',
    changeVerificationHint:
      'The existing Bark device is notified before a change is saved. If delivery fails, the current settings stay in effect.',
    configured: 'Configured',
    notConfigured: 'Not configured',
    enabled: 'Enable push',
    save: 'Save',
    clear: 'Clear',
    saved: 'Saved',
    cleared: 'Cleared',
  },
}
