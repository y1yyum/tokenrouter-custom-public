package provider

// parameter_evidence.go names the provider metadata shared between route
// scheduling and forwarding. A record supplies evidence for its own exact
// model and protocol tuples, so provider duplication discards the evidence.

// RequestParameterEvidenceExtraKey identifies versioned facts about exact provider routes.
const RequestParameterEvidenceExtraKey = "request_parameter_evidence"
