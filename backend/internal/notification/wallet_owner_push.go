package notification

import (
	"context"

	"github.com/TokenFlux/TokenRouter/internal/notification/bark"
)

// wallet_owner_push.go delivers sensitive wallet notifications to the selected
// owner of the custodial wallet. Delivery consults that account’s enabled
// device and active administrator status before reporting success to the
// wallet operation or to the persistent security monitor.

// OwnerPushAdapter routes private wallet alerts exclusively to the primary
// administrator and requires a real delivery attempt to succeed.
type OwnerPushAdapter struct {
	svc     *UserBarkService
	ownerID int64
}

func NewOwnerPushAdapter(svc *UserBarkService, ownerID int64) *OwnerPushAdapter {
	return &OwnerPushAdapter{svc: svc, ownerID: ownerID}
}

func (a *OwnerPushAdapter) PushToAdmins(ctx context.Context, title, body, group string) error {
	if a == nil || a.svc == nil || a.ownerID <= 0 {
		return ErrBarkNotConfigured
	}
	return a.svc.SendToUserRequired(ctx, a.ownerID, bark.Notification{
		Title: title, Body: body, Group: group, Level: "timeSensitive",
	})
}

func (a *OwnerPushAdapter) WalletOperationNotificationsEnabled(ctx context.Context) (bool, error) {
	if a == nil || a.svc == nil || a.ownerID <= 0 {
		return false, ErrBarkNotConfigured
	}
	return a.svc.WalletOperationNotificationsEnabled(ctx, a.ownerID)
}
