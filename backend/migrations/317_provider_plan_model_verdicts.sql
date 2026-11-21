-- Keep saved quality verdicts after scheduled result retention prunes its inputs.
-- A passing verdict advances the same watermark as a restriction.
CREATE TABLE IF NOT EXISTS provider_plan_model_verdicts (
    result_id bigint PRIMARY KEY,
    provider_id bigint NOT NULL,
    plan_id bigint NOT NULL,
    policy_version bigint NOT NULL CHECK (policy_version > 0),
    requested_model text NOT NULL CHECK (requested_model <> ''),
    upstream_model text NOT NULL CHECK (upstream_model <> ''),
    started_at timestamptz NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    restricted boolean NOT NULL,
    reason text NOT NULL CHECK (reason <> ''),
    expires_at timestamptz,
    CHECK (expires_at IS NULL OR expires_at > started_at)
);

-- One owner cannot clear another owner's decision for the same provider/model.
CREATE TABLE IF NOT EXISTS provider_plan_model_state (
    provider_id bigint NOT NULL,
    plan_id bigint NOT NULL,
    upstream_model text NOT NULL,
    result_id bigint NOT NULL REFERENCES provider_plan_model_verdicts(result_id),
    policy_version bigint NOT NULL,
    started_at timestamptz NOT NULL,
    PRIMARY KEY (provider_id, upstream_model, plan_id)
);
