BEGIN;

ALTER TABLE hosted_service_executions
    ADD COLUMN expected_contract_hash TEXT,
    ADD COLUMN input_schema_fingerprint BYTEA,
    ADD CONSTRAINT hosted_service_executions_contract_hash_valid
        CHECK (expected_contract_hash IS NULL OR expected_contract_hash ~ '^hct:v1:[a-f0-9]{64}$'),
    ADD CONSTRAINT hosted_service_executions_schema_fingerprint_valid
        CHECK (input_schema_fingerprint IS NULL OR octet_length(input_schema_fingerprint) = 32);

COMMIT;
