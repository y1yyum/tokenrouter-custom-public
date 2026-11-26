package openaiattempt

import (
	"context"
	"errors"
	"math"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/TokenFlux/TokenRouter/internal/billing"
	"github.com/TokenFlux/TokenRouter/internal/gateway/completion"
	"github.com/TokenFlux/TokenRouter/internal/gateway/forward"
	"github.com/TokenFlux/TokenRouter/internal/gateway/provider"
	"github.com/TokenFlux/TokenRouter/internal/infra/telemetry"
)

// responses_settlement.go owns the final metering decision for synchronous
// conversions. Reader evidence belongs to an individual upstream exchange.
// The attempt loop can discard a rejected exchange, but only its logical
// request exit submits trusted metering and publishes a final observation.
//
// Zero or uncertain metering remains searchable in the request log without
// creating a usage or funding entry. A billable observation freezes a priced
// command before the completion queue takes ownership. Queue rejection uses
// that same command inline, so a different execution context cannot choose
// new prices or reconstruct metering from a rejected terminal frame.

// observeResponsesSettlement retains the converted exchange's evidence or its
// typed failure when the transport returned no result. The return value marks
// the current exchange so other completion paths can defer to this owner.
func (b *responsesAttemptBridge) observeResponsesSettlement(err error, at time.Time) bool {
	classified, _ := errors.AsType[*forward.UpstreamFailoverError](err)
	convertedFailure := classified != nil && classified.Reason == forward.OpenAIResponsesCompatibilityReason
	if !convertedFailure && (b.result == nil || b.result.ResponsesSettlement == nil) {
		return false
	}
	b.settlementSeen = true
	b.settlementFailed = err != nil
	b.settlementPricingAt = at
	b.settlementFinal = telemetry.ResponsesSettlementObservation{
		Evidence: "missing", Disposition: "pending_review", Resolution: "no_result",
		ProviderID: b.provider.Record.ID,
	}
	if convertedFailure && classified.NextProviderAction == forward.NextProviderRetry {
		b.settlementFinal.Disposition = "not_executed"
	}
	if b.result != nil && b.result.ResponsesSettlement != nil {
		evidence := b.result.ResponsesSettlement
		b.settlementFinal.Evidence = evidence.State
		b.settlementFinal.Source = evidence.Source
		b.settlementFinal.Resolution = evidence.Resolution
	}
	return true
}

// completeConvertedResponses freezes request identity and priced work. The
// submission binding treats this work as mandatory even for text-only usage.
func (b *responsesAttemptBridge) completeConvertedResponses(result *forward.OpenAIResult) {
	input := b.captureCompletion(provider.OpenAICapture{
		Result:             result,
		RequestPayloadHash: billing.HashUsageRequestPayload(b.body),
		PricingUsageFields: b.groupMapping.ToUsageFields(b.reqModel, result.UpstreamModel),
		PricingAt:          b.settlementPricingAt,
	})
	b.settlementFinal.Disposition, b.settlementFinal.Evidence, b.settlementFinal.Source = resolveResponsesSettlement(input.Result, result.ResponsesSettlement)
	if b.settlementFinal.Disposition != "chargeable" {
		return
	}
	// Provider health follows the delivery lifecycle. Failed-but-metered results
	// must not reset failure protection as a side effect of pricing.
	if input.Provider != nil && b.settlementFailed {
		input.Provider.ResetForbiddenCounterOnSuccess = false
	}
	commit, err := b.binding().recorder.PrepareOpenAI(b.Context(), input)
	if err != nil {
		b.settlementFinal.Disposition = "pending_review"
		b.reqLog.Error("openai.responses_settlement_prepare_failed", zap.Error(err))
		return
	}
	log := b.reqLog
	b.binding().submitOpenAIUsageRecordTask(b.c, result, func(ctx context.Context) {
		if err := commit(ctx); err != nil {
			log.Error("openai.responses_settlement_failed", zap.Error(err))
		}
	})
}

// finishResponsesSettlement runs after final error or output delivery. If a
// terminal retry branch bypassed ordinary completion, the retained converted
// result is completed here. The request logger persists the same observation
// for an empty result, including transport failures and pre-execution refusals.
func (b *responsesAttemptBridge) finishResponsesSettlement() {
	if !b.settlementSeen {
		return
	}
	if !b.completionCalled && b.result != nil && b.result.ResponsesSettlement != nil {
		b.Complete()
	}
	final := b.settlementFinal
	if b.completionCalled {
		final.CompletionCount = 1
		if b.result != nil && b.result.ResponsesSettlement == nil {
			final.Disposition = "legacy_completion"
			final.Evidence = ""
			final.Source = ""
			final.Resolution = "fallback"
			final.ProviderID = b.provider.Record.ID
		}
	}
	switch {
	case b.Context().Err() != nil && !b.c.Writer.Written():
		final.Delivery = "client_gone"
	case b.c.Writer.Status() >= http.StatusBadRequest:
		final.Delivery = "error"
	case b.c.Writer.Written():
		final.Delivery = "response"
	default:
		final.Delivery = "no_response"
	}
	telemetry.UpdateRequest(b.Context(), func(record *telemetry.RequestRecord) {
		record.ResponsesSettlement = final
	})
}

// resolveResponsesSettlement gates pricing on positive observed work. Invalid
// token counters are cleared before pricing independently measured media or
// tools so uncertainty in one counter cannot add to another's charge.
func resolveResponsesSettlement(result *completion.Result, evidence *forward.ResponsesSettlementEvidence) (disposition, state, source string) {
	state, source = evidence.State, evidence.Source
	if evidence.ReplayAllowed {
		return "not_executed", state, source
	}
	if state == "" {
		state = "missing"
	}
	usage := result.Usage
	tokens := []int{
		usage.InputTokens, usage.OutputTokens, usage.CacheCreationInputTokens, usage.CacheReadInputTokens,
		usage.CacheCreation5mTokens, usage.CacheCreation1hTokens, usage.ImageInputTokens, usage.ImageOutputTokens, usage.ImageCacheReadTokens,
	}
	for _, count := range []*int{usage.CacheCreation30mTokens, usage.CacheCreation12hTokens} {
		if count != nil {
			tokens = append(tokens, *count)
		}
	}
	positive := false
	for _, count := range tokens {
		if count < 0 {
			state = "unverified"
			break
		}
		positive = positive || count > 0
	}
	trustedTokens := state == "reported" || (result.NativeUsage && state == "missing")
	if !trustedTokens {
		result.Usage = completion.TokenUsage{}
	}
	for _, count := range []int{result.ImageCount, result.ImageFrameCount, result.VideoCount, result.VideoDurationSeconds, result.WebSearchCalls, result.SearchCount} {
		if count < 0 {
			return "pending_review", "unverified", source
		}
	}
	var audio float64
	if result.AudioUsage != nil {
		audio = result.AudioUsage.DurationOrUnits
		if audio < 0 || math.IsNaN(audio) || math.IsInf(audio, 0) {
			return "pending_review", "unverified", source
		}
	}
	switch {
	case trustedTokens && positive:
		if result.NativeUsage {
			return "chargeable", "reported", "native"
		}
		return "chargeable", state, source
	case result.ImageCount > 0:
		return "chargeable", state, "image_output"
	case result.VideoCount > 0 || result.WebSearchCalls > 0 || result.SearchCount > 0 || audio > 0:
		return "chargeable", state, "operation"
	case state == "zero":
		return "zero", state, source
	default:
		return "pending_review", state, source
	}
}
