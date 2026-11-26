package responsecompat

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/TokenFlux/TokenRouter/internal/pkg/apperror"
	"github.com/TokenFlux/TokenRouter/internal/settings"
)

// service_test.go separates storage failures from post-commit publication
// failures while observing the request-side snapshot and administrator status.

// compatibilityRepository controls one settings record for sequential failure tests.
type compatibilityRepository struct {
	settings.Repository        // unused operations outside single-key conditional writes
	value               string // raw value exposed by the fixture
	writeError          error  // next conditional write failure
}

// TestCompatibilityServiceReportsFailedSaveAndFailedPublication verifies that
// only the post-commit failure exposes the new saved revision.
func TestCompatibilityServiceReportsFailedSaveAndFailedPublication(t *testing.T) {
	repo := &compatibilityRepository{}
	service := New(settings.New(repo))
	tuple := Tuple{EffectiveGroupID: 28, ProviderID: 57, UpstreamModel: "gpt-6-luna", UpstreamEndpoint: "/v1/responses"}
	config := Config{Mode: Enforce, Rules: []Rule{{Tuple: tuple, Mode: Enforce, SyncToStream: true, EnforcePercent: 100}}}
	require.Equal(t, Decision{Mode: Off}, service.Match(tuple, "logical-0"))

	repo.writeError = errors.New("fixture write failure")
	_, err := service.Update(t.Context(), config)
	require.ErrorIs(t, err, repo.writeError)
	status, err := service.Get(t.Context())
	require.NoError(t, err)
	require.Zero(t, status.Settings.Revision)
	require.Nil(t, status.AppliedRevision)

	repo.writeError = nil
	// Keep a newer local revision to simulate a restored database whose next
	// committed version cannot replace this instance's already published policy.
	require.NoError(t, service.publish(&snapshot{revision: 4, mode: Off}))
	_, err = service.Update(t.Context(), config)
	require.Equal(t, "SETTINGS_APPLY_FAILED", apperror.Reason(err))
	require.Equal(t, "true", apperror.FromError(err).Metadata["persisted"])
	require.Equal(t, Decision{Mode: Off, Revision: 4}, service.Match(tuple, "logical-0"))
	status, err = service.Get(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 1, status.Settings.Revision)
	require.NotNil(t, status.AppliedRevision)
	require.EqualValues(t, 4, *status.AppliedRevision)
}

// TestCompatibilityServiceDoesNotApplyReadback proves a normal administrator
// read cannot enable a cold instance or create a dependency from Match to storage.
func TestCompatibilityServiceDoesNotApplyReadback(t *testing.T) {
	repo := &compatibilityRepository{value: `{"revision":1,"mode":"enforce","rules":[{"effective_group_id":28,"provider_id":57,"upstream_model":"gpt-6-luna","upstream_endpoint":"/v1/responses","mode":"enforce","sync_to_stream":true,"enforce_percent":100}]}`}
	service := New(settings.New(repo))
	tuple := Tuple{EffectiveGroupID: 28, ProviderID: 57, UpstreamModel: "gpt-6-luna", UpstreamEndpoint: "/v1/responses"}
	status, err := service.Get(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 1, status.Settings.Revision)
	require.Nil(t, status.AppliedRevision)
	require.Equal(t, Decision{Mode: Off}, service.Match(tuple, "logical-0"))
	require.Equal(t, Decision{Mode: Off}, New(nil).Match(tuple, "logical-0"))

	repo.value = `{"revision":2,"mode":"unsupported"}`
	_, err = service.Get(t.Context())
	require.Equal(t, "INVALID_STORED_RESPONSES_COMPATIBILITY", apperror.Reason(err))
	require.Equal(t, Decision{Mode: Off}, service.Match(tuple, "logical-0"))
}

// GetValue supplies the fixture row or the repository's missing-key error.
func (r *compatibilityRepository) GetValue(context.Context, string) (string, error) {
	if r.value == "" {
		return "", settings.ErrSettingNotFound
	}
	return r.value, nil
}

// CompareAndSetMultiple stores the replacement unless the fixture injects
// a write failure. The database scenario checks the real CAS expectations.
func (r *compatibilityRepository) CompareAndSetMultiple(_ context.Context, values map[string]string, _ map[string]*string) error {
	if r.writeError != nil {
		return r.writeError
	}
	r.value = values[SettingKey]
	return nil
}
