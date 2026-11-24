-- Store external call aggregates separately from billing and active probe rows.
-- The production import created the same table before the application reader shipped.
CREATE TABLE IF NOT EXISTS public.marketplace_imported_call_minutes (
    source text NOT NULL,
    group_id bigint NOT NULL REFERENCES public.groups(id) ON DELETE CASCADE,
    source_group_id bigint NOT NULL CHECK (source_group_id > 0),
    bucket_start timestamptz NOT NULL CHECK (bucket_start = date_trunc('minute', bucket_start)),
    source_platform text NOT NULL CHECK (source_platform <> ''),
    model text NOT NULL CHECK (model <> ''),
    success_requests bigint NOT NULL CHECK (success_requests >= 0),
    error_requests bigint NOT NULL CHECK (error_requests >= 0),
    ignored_error_requests bigint NOT NULL CHECK (ignored_error_requests >= 0),
    input_tokens bigint NOT NULL CHECK (input_tokens >= 0),
    output_tokens bigint NOT NULL CHECK (output_tokens >= 0),
    cache_creation_tokens bigint NOT NULL CHECK (cache_creation_tokens >= 0),
    cache_read_tokens bigint NOT NULL CHECK (cache_read_tokens >= 0),
    ttft_sum_ms bigint NOT NULL CHECK (ttft_sum_ms >= 0),
    ttft_count bigint NOT NULL CHECK (ttft_count >= 0),
    duration_sum_ms bigint NOT NULL CHECK (duration_sum_ms >= 0),
    duration_count bigint NOT NULL CHECK (duration_count >= 0),
    CHECK (ignored_error_requests <= error_requests),
    CHECK (success_requests + error_requests > 0),
    PRIMARY KEY (source, group_id, bucket_start, source_platform, model)
);

CREATE INDEX IF NOT EXISTS idx_marketplace_imported_calls_group_time
    ON public.marketplace_imported_call_minutes (group_id, bucket_start);

COMMENT ON TABLE public.marketplace_imported_call_minutes IS
    'Historical external call counts and usage per group, model, and UTC minute. These facts are separate from TokenRouter billing and active probes.';
COMMENT ON COLUMN public.marketplace_imported_call_minutes.ignored_error_requests IS
    'Subset of error_requests ignored for health scoring by the source monitor configuration when exported.';
