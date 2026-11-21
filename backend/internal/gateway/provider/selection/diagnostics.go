package selection

import (
	"context"
	"fmt"
	"strings"
	"time"

	gatewayprovider "github.com/TokenFlux/TokenRouter/internal/gateway/provider"
	"github.com/TokenFlux/TokenRouter/internal/gateway/requeststate"
	"github.com/TokenFlux/TokenRouter/internal/protocol"
	providercore "github.com/TokenFlux/TokenRouter/internal/provider"
	"github.com/TokenFlux/TokenRouter/internal/routing"
	schedulercore "github.com/TokenFlux/TokenRouter/internal/scheduler"
	"github.com/TokenFlux/TokenRouter/internal/scheduler/policy"
	"github.com/TokenFlux/TokenRouter/internal/upstream/anthropic"
)

// diagnostics.go supplies production eligibility observations and settings to
// scheduler score diagnostics.
// Scheduling diagnostics explain provider scores using the parameters and
// feedback bound to production selectors. Before scoring, the adapter supplies
// group, cost-window, RPM, and quota settings needed by platform eligibility
// checks; candidate observation does not acquire a provider slot, register a
// session, persist health mutations, or schedule background quota refreshes.
//
// Group protocol admission, API-key quota exhaustion, and read-only scheduling
// thresholds run before model-window checks so their rejection reasons remain
// distinct from model cooldowns. Compatible providers then evaluate mapped
// model rules, quota pauses, Grok team and Free OAuth limits, credential
// blocks, proxy quarantine, shadow parent health, cost/RPM windows, and
// upstream group restrictions; non-compatible targets apply the generic
// selector's model, window, and group rules.

// GetOverview delegates the provider's scoring overview to a fresh diagnostic
// core. Callers supply the constructed Diagnostics instance; the returned
// result and source errors describe the provider's groups and effective
// advanced scheduling settings.
func (s *Diagnostics) GetOverview(ctx context.Context, id int64) (*policy.AdvancedSchedulerScoreDiagnosticResponse, error) {
	core, _ := s.diagnosticCore()
	return core.GetOverview(ctx, id)
}

// GetDetail delegates a scoring simulation for the provider and supplied group
// request. The core uses a fresh target association scope with the
// production-bound settings and feedback, then returns its detail or source
// error.
func (s *Diagnostics) GetDetail(ctx context.Context, id int64, request policy.AdvancedSchedulerScoreDiagnosticRequest) (*policy.AdvancedSchedulerScoreDiagnosticResponse, error) {
	core, scope := s.diagnosticCore()
	if request.ClientProtocol == "" && request.HTTPResponses && strings.TrimSpace(request.RequestedModel) != "" {
		request.ClientProtocol = string(protocol.ProtocolOpenAIResponses)
	}
	entry := protocol.ProtocolID(request.ClientProtocol)
	if entry == "" {
		if request.SourceGroupID != 0 {
			return nil, fmt.Errorf("source_group_id requires client_protocol")
		}
		return core.GetDetail(ctx, id, request)
	}
	switch entry {
	case protocol.ProtocolOpenAIResponses, protocol.ProtocolOpenAIChatCompletions, protocol.ProtocolAnthropicMessages, protocol.ProtocolResponsesCompact:
	default:
		return nil, fmt.Errorf("unsupported client_protocol")
	}
	request.RequestedModel = strings.TrimSpace(request.RequestedModel)
	if request.RequestedModel == "" {
		return nil, fmt.Errorf("requested_model is required for route preview")
	}
	origin := request.SourceGroupID
	if origin == 0 {
		origin = request.GroupID
	}
	if origin != request.GroupID {
		if origin < 1 || s.source == nil {
			return nil, fmt.Errorf("invalid source_group_id")
		}
		source, err := s.source.GetGroup(ctx, origin)
		if err != nil {
			return nil, err
		}
		if source == nil || !source.IsActive() || !source.AllowsClientProtocol(entry) || !source.ClaudeCodeOnly || source.FallbackGroupID == nil || *source.FallbackGroupID != request.GroupID {
			return nil, fmt.Errorf("source_group_id has no matching client fallback")
		}
	}
	check := &policy.PlanModelCheck{
		Enabled: s.planModelRestrictions != nil, CheckedAt: time.Now().UTC(),
		SourceGroupID: origin, GroupID: request.GroupID, ClientProtocol: string(entry), RequestedModel: request.RequestedModel,
	}
	var compact *gatewayprovider.CompactModels
	if entry == protocol.ProtocolResponsesCompact {
		compact = &s.compactModels
	}
	scope.requestedGroupID = request.GroupID
	// Share one set of owner observations between the hard-filter explanation
	// and the selected provider's detail, including when a prior gate excluded it.
	type target struct {
		id    int64
		model string
	}
	type observation struct {
		owners []providercore.ActivePlanModelRestriction
		err    error
	}
	seen := make(map[target]observation)
	if check.Enabled {
		read := gatewayprovider.PlanModelRestrictionReader(func(ctx context.Context, providerID int64, model string, _ time.Time) ([]providercore.ActivePlanModelRestriction, error) {
			key := target{providerID, model}
			if owners, ok := seen[key]; ok {
				return owners.owners, owners.err
			}
			owners, err := s.planModelRestrictions(ctx, providerID, model, check.CheckedAt)
			seen[key] = observation{owners: owners, err: err}
			return owners, err
		})
		ctx = read.WithRequest(ctx, compact, false)
	}
	result, err := core.GetDetail(ctx, id, request)
	if err != nil || result == nil || result.Detail == nil {
		return result, err
	}
	value, group := scope.target, scope.targetGroup
	if value != nil && group != nil {
		ctx = requeststate.WithGroup(ctx, group)
		ctx = requeststate.WithClientProtocol(ctx, entry)
		route := s.resolveDiagnosticRoutingModel(ctx, value, group, strings.TrimSpace(request.RequestedModel))
		check.RoutingModel = route
		wire, ok := diagnosticHTTPRoute(ctx, value, group, entry, route)
		if ok {
			check.UpstreamProtocol = string(wire)
			if value.Record.Platform == providercore.PlatformOpenAI {
				check.ActualModel = gatewayprovider.PlanModelTarget(ctx, value, route, compact)
			} else {
				check.ActualModel = gatewayprovider.ExecutionModelPolicy(value).UpstreamModel(ctx, route)
			}
		}
		if check.Enabled && ok {
			if reason := gatewayprovider.PlanModelRejection(ctx, value, route); reason == "plan_model_read_failed" {
				return nil, fmt.Errorf("read plan model restrictions for provider %d", id)
			}
			for _, owner := range seen[target{id, check.ActualModel}].owners {
				check.Owners = append(check.Owners, policy.PlanModelOwner{
					PlanID: owner.PlanID, ResultID: owner.ResultID,
					PolicyVersion: owner.PolicyVersion, StartedAt: owner.StartedAt, Reason: owner.Reason,
				})
			}
		}
	}
	result.Detail.PlanModelCheck = check
	return result, nil
}

// effectiveSettings reads global runtime settings and resolves the group's
// advanced overrides through Parameters. Missing diagnostics or parameter
// bindings use Parameters defaults, and basic or absent groups supply empty
// override values.
func (s *Diagnostics) effectiveSettings(ctx context.Context, group *routing.Group) (policy.EffectiveSettings, policy.RuntimeSettings) {
	var parameters *schedulercore.Parameters
	if s != nil {
		parameters = s.schedulerParameters
	}
	runtime := parameters.Runtime(ctx)
	return parameters.Effective(ctx, schedulerGroupOverrides(group)), runtime
}

// prepareEligibilityContext supplies group and batch cost/RPM observations
// through the bound generic selector, then compatible quota settings when a
// group exists. Group policy is stored in request state even when the generic
// selector is absent or the group is unhydrated in tests.
func (s *Diagnostics) prepareEligibilityContext(ctx context.Context, group *routing.Group, providers []gatewayprovider.ExecutionProvider) context.Context {
	if group != nil {
		ctx = requeststate.WithGroup(ctx, group)
	}
	if s == nil {
		return ctx
	}
	if s.gatewayService != nil {
		ctx = s.gatewayService.withGroupContext(ctx, group)
		ctx = s.gatewayService.withWindowCostPrefetch(ctx, providers)
		ctx = s.gatewayService.withRPMPrefetch(ctx, providers)
	}
	if s.openAIGateway != nil && group != nil {
		ctx = s.openAIGateway.withOpenAIQuotaAutoPauseContext(ctx)
	}
	return ctx
}

// diagnosticPlatformFilterReason returns the first platform-specific diagnostic
// rejection, or empty when those gates pass. Callers supply a usable target and
// group after the core's general hard filters; the requested model is trimmed
// and resolved through single-hop group mapping before provider inspection.
//
// Group protocol routes, persistent API-key quota exhaustion, and read-only
// scheduling thresholds precede ModelPolicy.Schedulable so they are not
// misreported as model cooldowns. Compatible and generic branches then check
// model support, quota pauses, Grok team and Free OAuth limits, runtime blocks,
// proxy quarantine, shadow health, window-cost/RPM gates, and upstream group
// restrictions without mutating provider state.
func (s *Diagnostics) diagnosticPlatformFilterReason(
	ctx context.Context,
	provider *gatewayprovider.ExecutionProvider,
	group *routing.Group,
	request policy.AdvancedSchedulerScoreDiagnosticRequest,
	now time.Time,
) string {
	if provider == nil {
		return ""
	}
	if group != nil {
		if _, ok := requeststate.GroupFromContext(ctx); !ok {
			ctx = requeststate.WithGroup(ctx, group)
		}
	}
	requestedModel := strings.TrimSpace(request.RequestedModel)
	entry := protocol.ProtocolID(request.ClientProtocol)
	if entry != "" {
		ctx = requeststate.WithClientProtocol(ctx, entry)
	}
	routingModel := s.resolveDiagnosticRoutingModel(ctx, provider, group, requestedModel)

	if entry != "" && requestedModel != "" {
		if group != nil && group.ClaudeCodeOnly || request.SourceGroupID != 0 && request.SourceGroupID != request.GroupID && (group == nil || !group.IsActive()) {
			return "group_fallback_unavailable"
		}
		if _, ok := diagnosticHTTPRoute(ctx, provider, group, entry, routingModel); !ok {
			return "protocol_unavailable"
		}
		if reason := gatewayprovider.PlanModelRejection(ctx, provider, routingModel); reason != "" {
			return reason
		}
	}
	if !diagnosticSupportsGroupProtocols(ctx, provider, group, "") {
		return "protocol_unavailable"
	}
	// ModelPolicy.Schedulable rejects IsQuotaExceeded through IsSchedulable;
	// check persistent API-key and Bedrock quota exhaustion first to keep its
	// diagnostic reason distinct from model_runtime_blocked.
	if provider.View().IsAPIKeyOrBedrock() && provider.View().IsQuotaExceeded() {
		return "quota_exceeded"
	}
	if s.isBlockedByDiagnosticSchedulingThreshold(ctx, provider, routingModel, now) {
		return "scheduling_threshold"
	}

	if provider.View().IsOpenAICompatible() {
		if !gatewayprovider.ExecutionModelPolicy(provider).Schedulable(ctx, routingModel) {
			return "model_runtime_blocked"
		}
		if provider.View().IsOpenAI() {
			if paused, _ := gatewayprovider.OpenAIQuotaPause(ctx, provider); paused {
				return "quota_auto_pause"
			}
		}
		if provider.View().IsGrok() {
			if paused, _ := gatewayprovider.GrokQuotaPause(provider); paused {
				return "quota_auto_pause"
			}
		}
		if !gatewayprovider.ExecutionModelPolicy(provider).SupportsCompatibleRouting(ctx, routingModel) {
			return "model_unsupported"
		}
		if routingModel != "" && !diagnosticSupportsGroupProtocols(ctx, provider, group, routingModel) {
			return "protocol_unavailable"
		}
		if routingModel != "" && provider.View().IsGrokOAuth() {
			upstreamModel := gatewayprovider.ExecutionModelPolicy(provider).WithContext(ctx).CanonicalSchedulingModel(routingModel)
			if isGrokTeamModelRateLimited(provider, upstreamModel, now) {
				return "model_runtime_blocked"
			}
		}
		if s.isBlockedByDiagnosticGrokFreeQuota(provider) {
			return "grok_free_quota_exceeded"
		}
		if s != nil && s.openAIGateway != nil {
			if s.openAIGateway.isOpenAIProviderRequestRuntimeBlocked(ctx, provider, routingModel) {
				return "runtime_blocked"
			}
			if s.openAIGateway.isOpenAIProxyStreamQuarantined(ctx, provider) {
				return "proxy_stream_quarantined"
			}
			scheduler := &compatiblePicker{service: s.openAIGateway}
			if !providercore.ParentHealthyForShadow(gatewayprovider.ExecutionRecord(provider), func(id int64) *providercore.Record {
				return gatewayprovider.ExecutionRecord(scheduler.lookupShadowParentProvider(ctx, id))
			}) {
				return "shadow_parent_unhealthy"
			}
			if group != nil {
				groupID := group.ID
				if s.openAIGateway.NeedsUpstreamGroupRestriction(ctx, &groupID) &&
					s.openAIGateway.UpstreamRoutingModelRestricted(ctx, groupID, provider, routingModel, entry == protocol.ProtocolResponsesCompact) {
					return "group_upstream_restricted"
				}
			}
		}
		if s != nil && s.gatewayService != nil {
			isSticky := provider.Record.ID == request.StickyProviderID
			if !s.gatewayService.isProviderSchedulableForWindowCost(ctx, provider, isSticky) {
				return "window_cost_exceeded"
			}
			if !s.gatewayService.isProviderSchedulableForRPM(ctx, provider, isSticky) {
				return "rpm_exceeded"
			}
		}
		return ""
	}

	if s != nil && s.gatewayService != nil {
		if routingModel != "" && !s.gatewayService.isRoutingModelSupportedByProviderWithContext(ctx, provider, routingModel) {
			return "model_unsupported"
		}
		if routingModel != "" && !diagnosticSupportsGroupProtocols(ctx, provider, group, routingModel) {
			return "protocol_unavailable"
		}
		if !gatewayprovider.ExecutionModelPolicy(provider).Schedulable(ctx, routingModel) {
			return "model_runtime_blocked"
		}
		isSticky := provider.Record.ID == request.StickyProviderID
		if !s.gatewayService.isProviderSchedulableForWindowCost(ctx, provider, isSticky) {
			return "window_cost_exceeded"
		}
		if !s.gatewayService.isProviderSchedulableForRPM(ctx, provider, isSticky) {
			return "rpm_exceeded"
		}
		if s.isBlockedByDiagnosticGrokFreeQuota(provider) {
			return "grok_free_quota_exceeded"
		}
		if group != nil {
			groupID := group.ID
			if s.gatewayService.needsUpstreamGroupRestrictionCheck(ctx, &groupID) &&
				s.gatewayService.isUpstreamModelRestrictedByGroup(ctx, groupID, provider, requestedModel) {
				return "group_upstream_restricted"
			}
		}
		return ""
	}

	if routingModel != "" && !gatewayprovider.ExecutionModelPolicy(provider).Supports(ctx, routingModel) {
		return "model_unsupported"
	}
	if routingModel != "" && !diagnosticSupportsGroupProtocols(ctx, provider, group, routingModel) {
		return "protocol_unavailable"
	}
	if !gatewayprovider.ExecutionModelPolicy(provider).Schedulable(ctx, routingModel) {
		return "model_runtime_blocked"
	}
	return ""
}

// resolveDiagnosticRoutingModel maps a client model once through the active
// group's pricing policy. Blank models, nil groups, or unbound selectors keep
// the supplied model unchanged.
func (s *Diagnostics) resolveDiagnosticRoutingModel(ctx context.Context, provider *gatewayprovider.ExecutionProvider, group *routing.Group, requestedModel string) string {
	if s == nil || group == nil || requestedModel == "" {
		return requestedModel
	}
	groupID := group.ID
	if provider != nil && provider.View().IsOpenAICompatible() {
		if s.openAIGateway != nil {
			return s.openAIGateway.resolveGroupRoutingModel(ctx, &groupID, requestedModel)
		}
		if s.gatewayService != nil {
			return s.gatewayService.groupMappedModelForGroup(ctx, &groupID, requestedModel)
		}
		return requestedModel
	}
	if s.gatewayService != nil {
		return s.gatewayService.groupMappedModelForGroup(ctx, &groupID, requestedModel)
	}
	if s.openAIGateway != nil {
		return s.openAIGateway.resolveGroupRoutingModel(ctx, &groupID, requestedModel)
	}
	return requestedModel
}

// diagnosticSupportsGroupProtocols reports whether provider can serve at least
// one client protocol in group.AllowedProtocols. Unhydrated groups with a nil
// AllowedProtocols slice admit candidates, while an explicit empty selection
// rejects every candidate.
func diagnosticSupportsGroupProtocols(ctx context.Context, provider *gatewayprovider.ExecutionProvider, group *routing.Group, routingModel string) bool {
	if provider == nil || group == nil || group.AllowedProtocols == nil {
		return true
	}
	if len(group.AllowedProtocols) == 0 {
		return false
	}
	modelPolicy := gatewayprovider.ExecutionModelPolicy(provider).WithContext(ctx)
	snapshot := modelPolicy.CandidateSnapshot()
	effort := requeststate.RoutingReasoningEffortFromContext(ctx)
	for _, entry := range group.AllowedProtocols {
		if routingModel != "" && snapshot.ModelProtocols != nil {
			if _, ok := snapshot.ResolveModelRouteForEffort(routingModel, effort, entry, group.ProtocolFallbacks); ok {
				return true
			}
			continue
		}
		if _, ok := modelPolicy.ProtocolRoute(group, entry); ok {
			return true
		}
	}
	return false
}

// schedulingThresholds reads platform scheduling thresholds from the first
// configured health observer without triggering provider writes.
func (s *Diagnostics) schedulingThresholds(ctx context.Context) (map[string]int, bool) {
	if s == nil {
		return nil, false
	}
	if s.openAIGateway != nil && s.openAIGateway.healthObserver != nil && s.openAIGateway.healthObserver.Core != nil {
		if thresholds, ok := s.openAIGateway.healthObserver.Core.SchedulingThresholds(ctx); ok {
			return thresholds, true
		}
	}
	if s.gatewayService != nil && s.gatewayService.healthObserver != nil && s.gatewayService.healthObserver.Core != nil {
		return s.gatewayService.healthObserver.Core.SchedulingThresholds(ctx)
	}
	return nil, false
}

// isBlockedByDiagnosticSchedulingThreshold evaluates provider-wide and
// Anthropic Fable usage thresholds on a detached record copy without mutating
// persistent or in-memory scheduling state.
func (s *Diagnostics) isBlockedByDiagnosticSchedulingThreshold(ctx context.Context, provider *gatewayprovider.ExecutionProvider, routingModel string, now time.Time) bool {
	thresholds, ok := s.schedulingThresholds(ctx)
	if !ok || provider == nil {
		return false
	}
	evalNow := now.UTC()
	record := gatewayprovider.ExecutionRecord(provider)
	decision := providercore.EvaluateProviderSchedulingThreshold(record, thresholds, evalNow)
	if decision.ShouldPause && decision.Until != nil && decision.Until.After(evalNow) {
		return true
	}
	if routingModel == "" {
		return false
	}
	mappedModel, _, _ := gatewayprovider.ExecutionModelPolicy(provider).CandidateSnapshot().ModelPolicy.ResolveForEffort(routingModel, requeststate.RoutingReasoningEffortFromContext(ctx))
	if !anthropic.IsAnthropicFableModel(mappedModel) && !anthropic.IsAnthropicFableModel(routingModel) {
		return false
	}
	fableDecision := providercore.EvaluateAnthropicFableSchedulingThreshold(record, thresholds, evalNow)
	return fableDecision.ShouldPause && fableDecision.Until != nil && fableDecision.Until.After(evalNow)
}

// isBlockedByDiagnosticGrokFreeQuota checks cached Grok Free OAuth token-budget
// observations across basic and advanced gates without scheduling refreshes.
func (s *Diagnostics) isBlockedByDiagnosticGrokFreeQuota(provider *gatewayprovider.ExecutionProvider) bool {
	if s == nil || provider == nil || !providercore.IsExplicitGrokFreeOAuthProvider(gatewayprovider.ExecutionProtocolRecord(provider)) {
		return false
	}
	id := provider.Record.ID
	if s.openAIGateway != nil {
		if s.openAIGateway.freeQuotaGate.ObserveBlocked(id) {
			return true
		}
		if picker, ok := s.openAIGateway.ensureOpenAIProviderScheduler().(*compatiblePicker); ok {
			if picker.freeQuotaGate.Load().ObserveBlocked(id) {
				return true
			}
		}
	}
	return s.gatewayService != nil && s.gatewayService.freeQuotaGate.ObserveBlocked(id)
}

// diagnosticHTTPRoute resolves the model-specific conversion after destination
// group admission. Dedicated Compact also requires the group's Compact permission.
func diagnosticHTTPRoute(ctx context.Context, p *gatewayprovider.ExecutionProvider, group *routing.Group, entry protocol.ProtocolID, model string) (protocol.ProtocolID, bool) {
	if group == nil || !group.AllowsClientProtocol(entry) || !gatewayprovider.ExecutionModelPolicy(p).AllowsProtocol(ctx, model) {
		return "", false
	}
	policy := gatewayprovider.ExecutionModelPolicy(p).WithContext(ctx)
	route, ok := policy.CandidateSnapshot().ResolveModelRoute(model, entry, group.ProtocolFallbacks)
	if entry == protocol.ProtocolResponsesCompact && !gatewayprovider.AllowsCompatibleCompact(p) {
		return "", false
	}
	return route.Protocol, ok
}
