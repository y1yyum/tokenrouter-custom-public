package app

import (
	"context"

	"github.com/TokenFlux/TokenRouter/internal/app/lifecycle"
	"github.com/TokenFlux/TokenRouter/internal/gateway"
	"github.com/TokenFlux/TokenRouter/internal/gateway/requestpolicy"
	"github.com/TokenFlux/TokenRouter/internal/gateway/responsecompat"
	"github.com/TokenFlux/TokenRouter/internal/infra/telemetry/logging"
	"github.com/TokenFlux/TokenRouter/internal/server/runtimeconfig"
)

// runtime_settings.go registers startup migrations and the two compatibility
// pollers. Forwarded-IP loading and Grok default-model migration each log a
// warning on failure so catalog initialization can proceed. The rollout
// services independently read and publish their own database settings.

// settingsRuntimeReady marks registration of forwarding settings load and default-model migration.
type settingsRuntimeReady struct{}

// provideSettingsRuntime registers settings initialization before catalog
// loading in startup order. It attempts forwarded-IP loading followed by the
// Grok default-model migration, logs each error, and returns a registration
// marker before those startup operations execute. Call before manager startup
// or shutdown, while it still accepts hook registration.
// @project-doc docs/architecture/system_architecture.md#startup_and_shutdown
func provideSettingsRuntime(
	manager *lifecycle.Manager,
	settingService *gateway.RuntimeSettings,
	forwarded *runtimeconfig.ForwardedSettings,
	compatibility *responsecompat.Service,
	parameters *requestpolicy.Service,
) *settingsRuntimeReady {
	registerSettingsPoller(manager, "ResponsesCompatibilityRefresh", compatibility.Run)
	registerSettingsPoller(manager, "RequestParameterPolicyRefresh", parameters.Run)
	manager.Register(lifecycle.Hook{Name: "RuntimeSettingsInitialization", StartOrder: 181, StopOrder: shutdownStageSettings, Start: func(ctx context.Context) error {
		// Report a forwarded-IP load failure and still attempt the Grok migration.
		// Returning success lets the remaining application startup hooks proceed.
		if err := forwarded.LoadForwardedClientIPSettings(ctx); err != nil {
			logging.LegacyPrintf("service.setting", "Warning: load forwarded client IP settings failed: %v", err)
		}
		// Treat a default-model migration error as a startup warning. Other
		// services can start even when this settings migration did not complete.
		if err := settingService.MigrateGrokDefaultTextModel(ctx); err != nil {
			logging.LegacyPrintf("service.setting", "Warning: migrate Grok default text model failed: %v", err)
		}
		return nil
	}})
	return &settingsRuntimeReady{}
}

// registerSettingsPoller binds a policy refresh loop to application lifetime.
// Register before startup; the loop receives a distinct cancellation scope so
// each policy can poll independently and shutdown can wait for its read to exit.
func registerSettingsPoller(manager *lifecycle.Manager, name string, run func(context.Context)) {
	var cancelRefresh context.CancelFunc
	refreshDone := make(chan struct{})
	manager.Register(lifecycle.Hook{
		Name: name, StartOrder: 182, StopOrder: shutdownStageSettings,
		Start: func(ctx context.Context) error {
			// Startup context cancellation belongs to the startup sequence. The
			// settings shutdown hook owns the poller and joins its final database read.
			var runCtx context.Context
			runCtx, cancelRefresh = context.WithCancel(context.WithoutCancel(ctx))
			go func() {
				defer close(refreshDone)
				run(runCtx)
			}()
			return nil
		},
		Stop: func(ctx context.Context) error {
			if cancelRefresh == nil {
				return nil
			}
			cancelRefresh()
			select {
			case <-refreshDone:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	})
}
