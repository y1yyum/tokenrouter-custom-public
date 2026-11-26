package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"

	"github.com/TokenFlux/TokenRouter/internal/gateway/requestpolicy"
	"github.com/TokenFlux/TokenRouter/internal/gateway/responsecompat"
	"github.com/TokenFlux/TokenRouter/internal/infra/telemetry"
	upstreamopenai "github.com/TokenFlux/TokenRouter/internal/upstream/openai"
)

// compatibility_cohorts.go records rollout enrollment separately from
// upstream attempts and funding. The HTTP request owns its admission and
// delivery observations until its entry handler returns; a provider switch
// retains the original cohort and a canceled caller cannot cancel the final
// monitoring write.

const (
	responsesCohortKey  = "gateway_responses_cohort"
	parametersCohortKey = "gateway_parameters_cohort"
	cohortStoreTimeout  = time.Second
)

// compatibilityCohort keeps request-local state for the first matched rule.
// The HTTP execution goroutine owns these observations, including callbacks
// invoked synchronously by the final dispatch and downstream JSON writer.
type compatibilityCohort struct {
	entry           responsecompat.Cohort
	store           responsecompat.CohortRecorder
	applied         bool
	delivered       string
	rejectionSource string // final parameter rejection stage
	// stream holds accepted native SSE evidence for D2. Execution can report a
	// disconnect separately when its codec stopped without attempting a write.
	stream           *parameterDelivery
	clientDisconnect bool
	unaryServed      bool // generic fallback for completed unary output without a semantic marker
	// finalRevision keeps the last Responses decision, including revision zero
	// when the prepared attempt encounters an unavailable or stale snapshot.
	finalRevision int64
}

// beginCompatibilityCohort retains the initial identity across retries and
// persists the in-flight row before the outcome is available. A failed
// monitoring write is logged; finalization can still insert the complete row.
func beginCompatibilityCohort(c *gin.Context, key string, store responsecompat.CohortRecorder, entry responsecompat.Cohort) *compatibilityCohort {
	if store == nil {
		return nil
	}
	if existing := compatibilityCohortFrom(c, key); existing != nil {
		return existing
	}
	entry.EnrolledAt = time.Now()
	cohort := &compatibilityCohort{entry: entry, store: store}
	c.Set(key, cohort)
	if key == parametersCohortKey {
		value, _ := c.Get(requestTimingWriterKey)
		if writer, ok := value.(*timingWriter); ok {
			cohort.stream = &parameterDelivery{endpoint: entry.Tuple.UpstreamEndpoint}
			writer.parameterStream.Store(cohort.stream)
		}
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), cohortStoreTimeout)
	defer cancel()
	if err := store.Enroll(ctx, entry); err != nil {
		slog.WarnContext(ctx, "compatibility enrollment write failed", "request_id", entry.RequestID, "phase", entry.Phase, "error", err)
	}
	return cohort
}

// compatibilityCohortFrom returns the enrollment for one policy on this request.
func compatibilityCohortFrom(c *gin.Context, key string) *compatibilityCohort {
	value, _ := c.Get(key)
	cohort, _ := value.(*compatibilityCohort)
	return cohort
}

// responseJSONOutcome classifies the actual delivered JSON for the control
// arm using output and terminal evidence. A transport 200 by itself does not
// imply that a generation completed or that a refusal was delivered.
func responseJSONOutcome(body []byte) string {
	root := gjson.ParseBytes(body)
	visible, refused := upstreamopenai.ResponsesUsefulOutput(body)
	if refused && !visible {
		return responsecompat.OutcomeRefused
	}
	if visible {
		switch root.Get("status").String() {
		case "completed":
			return responsecompat.OutcomeCompleted
		case "incomplete":
			if root.Get("incomplete_details.reason").String() != "" {
				return responsecompat.OutcomeIncomplete
			}
		}
	}
	return responsecompat.OutcomeOtherFailure
}

// recordResponsesCohortDelivery retains the first final client delivery
// decision at the writer. Later cancellation cannot change an already delivered
// response, and recovered attempts never publish an outcome through this hook.
func recordResponsesCohortDelivery(c *gin.Context, outcome string) {
	for _, key := range []string{responsesCohortKey, parametersCohortKey} {
		if cohort := compatibilityCohortFrom(c, key); cohort != nil && cohort.delivered == "" {
			cohort.delivered = outcome
		}
	}
}

// finishCompatibilityCohorts freezes each enrolled phase independently. The
// final row uses the client's delivered outcome even if cancellation follows
// delivery; late upstream events collected during drain cannot replace it.
func finishCompatibilityCohorts(c *gin.Context) {
	for _, key := range []string{responsesCohortKey, parametersCohortKey} {
		cohort := compatibilityCohortFrom(c, key)
		if cohort == nil {
			continue
		}
		now := time.Now()
		start, ok := c.Request.Context().Value(telemetry.RequestStartedAt).(time.Time)
		if !ok {
			start = cohort.entry.EnrolledAt
		}
		outcome := cohort.delivered
		if outcome == "" && cohort.stream != nil {
			var writeFailed bool
			outcome, writeFailed = cohort.stream.snapshot()
			if outcome == responsecompat.OutcomeOtherFailure {
				status := c.Writer.Status()
				if streamError, ok := GetOpsStreamError(c); ok && streamError.IntendedStatus >= http.StatusBadRequest {
					status = streamError.IntendedStatus
				}
				outcome = parameterFailureOutcome(status)
			}
			if outcome == "" && writeFailed {
				outcome = responsecompat.OutcomeClientGone
			}
		}
		if outcome == "" && cohort.stream == nil && cohort.unaryServed {
			outcome = responsecompat.OutcomeCompleted
		}
		if outcome == "" {
			// Header commitment alone does not establish a delivered response. A
			// canceled synchronous client that only received SSE framing also has
			// no final JSON result to retain.
			bodyWritten := c.Writer.Size() > 0
			sse := CommittedSSE(c)
			status := c.Writer.Status()
			switch {
			case cohort.clientDisconnect || c.Request.Context().Err() != nil && (!bodyWritten || sse):
				outcome = responsecompat.OutcomeClientGone
			case bodyWritten && (key == parametersCohortKey || status == http.StatusBadRequest):
				outcome = parameterFailureOutcome(status)
			default:
				outcome = responsecompat.OutcomeOtherFailure
			}
		}
		message := strings.ToLower(c.GetString(OpsUpstreamErrorMessageKey))
		if key == parametersCohortKey && cohort.rejectionSource == "" && outcome == responsecompat.OutcomeParameter400 {
			if intent, ok := requestpolicy.FromContext(c.Request.Context()); ok && intent.LastUpstreamStatus() == http.StatusBadRequest {
				cohort.rejectionSource = responsecompat.RejectionUpstream
			}
		}
		result := responsecompat.CohortResult{
			Outcome: outcome, Applied: cohort.applied, FinishedAt: now, Duration: now.Sub(start),
			TargetParameterError: key == responsesCohortKey && outcome == responsecompat.OutcomeParameter400 && strings.Contains(message, "only supports streaming generation requests"),
			RejectionSource:      cohort.rejectionSource,
			FinalRevision:        cohort.finalRevision,
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), cohortStoreTimeout)
		err := cohort.store.Finish(ctx, cohort.entry, result)
		cancel()
		if err != nil {
			slog.WarnContext(c.Request.Context(), "compatibility final outcome write failed", "request_id", cohort.entry.RequestID, "phase", cohort.entry.Phase, "error", err)
		}
	}
}
