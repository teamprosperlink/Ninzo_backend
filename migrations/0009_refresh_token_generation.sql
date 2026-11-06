-- +goose Up
-- Each renewal is one generation further along its chain. A retry after a lost reply makes a
-- second token in the same generation, which is harmless. Two different tokens of one generation
-- both being used means two holders, so the session ends. See authentication_rules.md, section 4.
ALTER TABLE refresh_tokens ADD COLUMN generation integer NOT NULL DEFAULT 0 CHECK (generation >= 0);
CREATE INDEX refresh_tokens_chain_generation_idx ON refresh_tokens (chain_id, generation) WHERE used_at IS NOT NULL;

-- +goose Down
DROP INDEX refresh_tokens_chain_generation_idx;
ALTER TABLE refresh_tokens DROP COLUMN generation;
