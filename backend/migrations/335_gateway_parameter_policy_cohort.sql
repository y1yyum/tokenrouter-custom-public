-- Stage two has its own strict arm and records the source of a final rejection.
ALTER TABLE gateway_compatibility_cohorts
    DROP CONSTRAINT gateway_compatibility_cohorts_arm_check;

UPDATE gateway_compatibility_cohorts SET arm = 'strict' WHERE phase = 2 AND arm = 'enforce';

ALTER TABLE gateway_compatibility_cohorts
    ADD CONSTRAINT gateway_compatibility_cohorts_arm_check CHECK (
        (phase = 1 AND arm IN ('observe', 'enforce')) OR
        (phase = 2 AND arm IN ('observe', 'strict'))
    ),
    ADD COLUMN rejection_source text,
    ADD CONSTRAINT gateway_compatibility_cohorts_rejection_source_check CHECK (
        rejection_source IS NULL OR (
            phase = 2 AND outcome IS NOT NULL AND rejection_source IN ('entry', 'candidate', 'upstream')
        )
    );
