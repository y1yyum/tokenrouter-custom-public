package requestpolicy

import "github.com/TokenFlux/TokenRouter/internal/pkg/apperror"

// candidate_errors.go defines the two client outcomes of strict route screening.
// A field error requires a complete rejection of the authorized pool. Loss
// of capacity or route evidence uses an availability error.

const (
	// Candidate error reasons distinguish terminal field rejection from inability to verify availability.
	CandidateUnsupportedReason = "REQUEST_PARAMETER_UNSUPPORTED"
	CandidateUnavailableReason = "REQUEST_PARAMETER_ROUTE_UNAVAILABLE"
)

// CandidateUnsupported returns a client error naming the incompatible original fields.
func CandidateUnsupported(fields string) error {
	return apperror.BadRequest(CandidateUnsupportedReason, "No authorized route supports the request field(s): "+fields)
}

// CandidateUnavailable returns a retryable service error for unverified capacity.
func CandidateUnavailable() error {
	return apperror.ServiceUnavailable(CandidateUnavailableReason, "No verified route is available for the request parameters.")
}

// IsCandidateError identifies local field and availability outcomes from selection.
func IsCandidateError(err error) bool {
	reason := apperror.Reason(err)
	return reason == CandidateUnsupportedReason || reason == CandidateUnavailableReason
}
