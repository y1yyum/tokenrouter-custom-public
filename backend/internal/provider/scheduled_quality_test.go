package provider

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestScheduledQualityCalibration locks the accepted answer shape to the rule
// version. Unknown input and transport failures always preserve routing state.
func TestScheduledQualityCalibration(t *testing.T) {
	plan := &ScheduledTestPlan{ID: 9, ProviderID: 7, ModelID: "alias", QualityRevision: 3}
	for _, tc := range []struct{ name, status, body, model, grade, reason string }{
		{"correct", "success", " [15, 5, 21] ", "X", "pass", "all_answers_correct"},
		{"wrong integer", "success", "[15,5,22]", "X", "fail", "incorrect_answer"},
		{"empty", "success", " ", "X", "ungraded", "empty_response"},
		{"missing element", "success", "[15,5]", "X", "ungraded", "invalid_answer"},
		{"null element", "success", "[15,null,21]", "X", "ungraded", "invalid_answer"},
		{"prose", "success", "answer: [15,5,21]", "X", "ungraded", "invalid_answer"},
		{"failed stream", "failed", "[15,5,22]", "X", "ungraded", "transport_failed"},
		{"unknown mapping", "success", "[15,5,22]", "", "ungraded", "unknown_model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := gradeScheduledQuality(plan, &ScheduledTestResult{Status: tc.status, ResponseText: tc.body}, tc.model)
			require.Equal(t, tc.grade, q.Grade)
			require.Equal(t, tc.reason, q.Reason)
			require.Equal(t, qualityRule, q.Rule)
			require.Equal(t, plan.QualityRevision, q.Revision)
			require.Equal(t, plan.ModelID, q.RequestedModel)
		})
	}
}
