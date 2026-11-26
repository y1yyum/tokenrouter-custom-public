CREATE TABLE gateway_compatibility_cohorts (
    phase smallint NOT NULL CHECK (phase IN (1, 2)),
    request_id text NOT NULL CHECK (request_id <> ''),
    enrolled_at timestamptz NOT NULL,
    revision bigint NOT NULL CHECK (revision > 0),
    final_revision bigint CHECK (final_revision >= 0),
    arm text NOT NULL CHECK (arm IN ('observe', 'enforce')),
    group_id bigint NOT NULL CHECK (group_id > 0),
    provider_id bigint,
    model text NOT NULL,
    endpoint text NOT NULL,
    client_stream boolean NOT NULL,
    applied boolean NOT NULL DEFAULT false,
    outcome text CHECK (outcome IN (
        'completed', 'incomplete', 'refused', 'parameter_400', 'overload_503',
        'protocol_502', 'timeout_504', 'client_gone', 'other_failure'
    )),
    target_parameter_error boolean NOT NULL DEFAULT false,
    finished_at timestamptz,
    duration_ms bigint CHECK (duration_ms >= 0),
    PRIMARY KEY (phase, request_id),
    CHECK (phase <> 1 OR provider_id IS NOT NULL),
    CHECK ((finished_at IS NULL) = (outcome IS NULL)),
    CHECK ((duration_ms IS NULL) = (outcome IS NULL)),
    CHECK ((final_revision IS NOT NULL) = (phase = 1 AND outcome IS NOT NULL)),
    CHECK (NOT target_parameter_error OR outcome = 'parameter_400')
);

CREATE INDEX gateway_compatibility_cohorts_window_idx
    ON gateway_compatibility_cohorts (phase, enrolled_at, revision, arm);
