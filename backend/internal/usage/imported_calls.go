package usage

import (
	"context"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/pkg/apperror"
)

// imported_calls.go defines the read-only report for external call history.
// Imported rows describe aggregate observations without a local payer, Key,
// or settlement identity. Both administrator screens read the same counters.
// Latency keeps its sample counts so totals can be combined without averaging
// minute or group averages.

const (
	// ImportedCallSource identifies the source displayed by the report.
	ImportedCallSource = "raks"
)

// ImportedCallMetrics carries additive request, token, and latency counters.
// A zero latency sample count means the corresponding average is unavailable.
type ImportedCallMetrics struct {
	SuccessRequests     int64 `json:"success_requests"`      // successful calls observed
	ErrorRequests       int64 `json:"error_requests"`        // failed calls observed
	InputTokens         int64 `json:"input_tokens"`          // ordinary input tokens
	OutputTokens        int64 `json:"output_tokens"`         // generated output tokens
	CacheCreationTokens int64 `json:"cache_creation_tokens"` // input tokens written to cache
	CacheReadTokens     int64 `json:"cache_read_tokens"`     // input tokens read from cache
	DurationSumMs       int64 `json:"duration_sum_ms"`       // total elapsed milliseconds
	DurationCount       int64 `json:"duration_count"`        // calls with elapsed samples
	TTFTSumMs           int64 `json:"ttft_sum_ms"`           // total time to first token in milliseconds
	TTFTCount           int64 `json:"ttft_count"`            // calls with first-token samples
}

// ImportedCallGroup contains a current group name and its selected counters.
type ImportedCallGroup struct {
	ImportedCallMetrics
	GroupID   int64  `json:"group_id"`   // destination group identifier
	GroupName string `json:"group_name"` // current destination group name
}

// ImportedCallMinute represents one stored minute, platform, group, and model.
type ImportedCallMinute struct {
	ImportedCallGroup
	BucketStart    time.Time `json:"bucket_start"`    // inclusive minute start
	SourcePlatform string    `json:"source_platform"` // platform recorded by the import
	Model          string    `json:"model"`           // model recorded by the import
}

// ImportedCallQuery selects optional half-open bounds and exact matches.
// WithMinutes includes the requested page and leaves group choices scoped to
// the full date range. A zero GroupID or empty Model selects any value.
type ImportedCallQuery struct {
	Start, End  *time.Time // optional inclusive start and exclusive end
	GroupID     int64      // optional destination group
	Model       string     // optional exact model name
	WithMinutes bool       // include a minute page and exact row count
	Page        int        // one-based minute page
	PageSize    int        // positive maximum minute rows per page
}

// ImportedCallReport contains group summaries and optional paged minute rows.
// Total counts minute rows in detail mode and groups in group mode.
type ImportedCallReport struct {
	Source   string               `json:"source"`    // original system identifier
	Groups   []ImportedCallGroup  `json:"groups"`    // grouped totals or date-scoped group choices
	Items    []ImportedCallMinute `json:"items"`     // selected minute rows
	Total    int64                `json:"total"`     // exact count for the selected view
	Page     int                  `json:"page"`      // selected page
	PageSize int                  `json:"page_size"` // requested minute page size
}

// ImportedCallReader is the optional storage capability for historical reports.
type ImportedCallReader interface {
	// ListImportedCalls reads the requested views from a consistent snapshot.
	ListImportedCalls(context.Context, ImportedCallQuery) (*ImportedCallReport, error)
}

// ListImportedCalls reads external observations through the configured reader.
// A missing reader is reported as unavailable so it cannot look like an empty
// import.
func (s *UsageService) ListImportedCalls(ctx context.Context, query ImportedCallQuery) (*ImportedCallReport, error) {
	if s == nil || s.readers.ImportedCallReader == nil {
		return nil, apperror.ServiceUnavailable("IMPORTED_CALLS_UNAVAILABLE", "Imported call history is unavailable")
	}
	return s.readers.ImportedCallReader.ListImportedCalls(ctx, query)
}
