-- +goose Up
-- Menu items. Prices are in paise. The console changes all of this later, so nothing is in code.
CREATE TABLE menu_items (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text        NOT NULL UNIQUE CHECK (length(name) BETWEEN 1 AND 80),
    description text        CHECK (length(description) <= 500),
    price_paise integer     NOT NULL CHECK (price_paise BETWEEN 100 AND 1000000),
    currency    text        NOT NULL DEFAULT 'INR' CHECK (currency = 'INR'),
    allergens   text[]      NOT NULL DEFAULT '{}' CHECK (cardinality(allergens) <= 20 AND array_position(allergens, NULL) IS NULL),
    photo_url   text        CHECK (photo_url ~ '^https://' AND length(photo_url) <= 1000),
    status      text        NOT NULL DEFAULT 'hidden' CHECK (status IN ('active', 'hidden')),
    sort_order  integer     NOT NULL DEFAULT 100,
    is_test     boolean     NOT NULL DEFAULT false,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE menu_items ENABLE ROW LEVEL SECURITY;

-- Every price an item has had, so an order can always show what it cost then.
CREATE TABLE price_history (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    item_id     uuid        NOT NULL REFERENCES menu_items (id) ON DELETE RESTRICT,
    price_paise integer     NOT NULL CHECK (price_paise BETWEEN 100 AND 1000000),
    reason      text        NOT NULL CHECK (length(reason) BETWEEN 3 AND 500),
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX price_history_item_idx ON price_history (item_id, created_at DESC);
ALTER TABLE price_history ENABLE ROW LEVEL SECURITY;
CREATE TRIGGER price_history_append_only BEFORE UPDATE OR DELETE ON price_history
    FOR EACH ROW EXECUTE FUNCTION forbid_change();
CREATE TRIGGER price_history_no_truncate BEFORE TRUNCATE ON price_history
    FOR EACH STATEMENT EXECUTE FUNCTION forbid_change();

-- A price change always leaves a history row, whoever makes it, so the history cannot be skipped.
-- +goose StatementBegin
CREATE FUNCTION record_price() RETURNS trigger
LANGUAGE plpgsql
SET search_path = ''
AS $$
BEGIN
    IF TG_OP = 'INSERT' OR NEW.price_paise IS DISTINCT FROM OLD.price_paise THEN
        INSERT INTO public.price_history (item_id, price_paise, reason)
        VALUES (NEW.id, NEW.price_paise, coalesce(nullif(current_setting('ninzo.price_reason', true), ''), 'no reason given'));
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION record_price() FROM PUBLIC;
CREATE TRIGGER menu_items_record_price AFTER INSERT OR UPDATE OF price_paise ON menu_items
    FOR EACH ROW EXECUTE FUNCTION record_price();

-- +goose Down
DROP TABLE price_history;
DROP TABLE menu_items;
DROP FUNCTION record_price();
