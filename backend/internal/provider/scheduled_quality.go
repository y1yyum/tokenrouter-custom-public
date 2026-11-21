package provider

import (
	"encoding/json"
	"strings"
	"time"
)

// A quality plan tests only the Responses text path. The fixed version names both
// the prompt and the grading rule. Any sample or parser change needs a new version.
const (
	qualityPrompt  = "Return only a JSON array of three integers: [8+7, 9-4, 3*7], with each expression evaluated."
	qualityRule    = "arithmetic-array-v1"
	qualityVersion = 1
	// QualityRestrictionTTL limits the effect of a failed probe after a plan stops.
	QualityRestrictionTTL = 24 * time.Hour
)

// ScheduledQualityObservation records exactly which plan revision and wire model
// produced a score. Ungraded results remain visible in the normal result history.
type ScheduledQualityObservation struct {
	ProviderID     int64  `json:"provider_id"`     // probe target at start
	RequestedModel string `json:"requested_model"` // plan alias at start
	UpstreamModel  string `json:"upstream_model"`  // accepted outbound Responses model
	Revision       int64  `json:"plan_revision"`   // configuration revision at start
	Rule           string `json:"rule"`            // identifies the prompt and expected response
	Version        int64  `json:"version"`         // monotonic policy watermark
	Grade          string `json:"grade"`           // pass, fail, or ungraded
	Reason         string `json:"reason"`          // stable explanation independent of response text
}

// gradeScheduledQuality accepts only three JSON integers from a successful,
// attributed exchange. Other response shapes carry no routing decision.
func gradeScheduledQuality(plan *ScheduledTestPlan, result *ScheduledTestResult, model string) *ScheduledQualityObservation {
	q := &ScheduledQualityObservation{ProviderID: plan.ProviderID, RequestedModel: plan.ModelID, UpstreamModel: model, Revision: plan.QualityRevision, Rule: qualityRule, Version: qualityVersion, Grade: "ungraded"}
	var answers []*int
	switch {
	case result.Status != "success":
		q.Reason = "transport_failed"
	case strings.TrimSpace(model) == "":
		q.Reason = "unknown_model"
	case strings.TrimSpace(result.ResponseText) == "":
		q.Reason = "empty_response"
	case json.Unmarshal([]byte(result.ResponseText), &answers) != nil || len(answers) != 3 || answers[0] == nil || answers[1] == nil || answers[2] == nil:
		q.Reason = "invalid_answer"
	case *answers[0] == 15 && *answers[1] == 5 && *answers[2] == 21:
		q.Grade = "pass"
		q.Reason = "all_answers_correct"
	default:
		q.Grade = "fail"
		q.Reason = "incorrect_answer"
	}
	return q
}
