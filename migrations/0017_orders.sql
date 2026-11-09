-- +goose Up
-- How long an unpaid order holds its stock (decision 0013). Adding the column with a default fills
-- it in every stored version, without editing any row. Then the default goes, so later zone rows
-- leave it unset unless they override it.
ALTER TABLE zone_settings ADD COLUMN payment_hold_s integer DEFAULT 600 CHECK (payment_hold_s BETWEEN 60 AND 3600);
ALTER TABLE zone_settings ALTER COLUMN payment_hold_s DROP DEFAULT;

-- Orders. state is the true state, which the customer never sees. Settings and gate versions are
-- kept, so a later change never alters a running order.
CREATE TABLE orders (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id       uuid        NOT NULL REFERENCES customers (id) ON DELETE RESTRICT,
    idempotency_key   text        NOT NULL,
    gate_id           uuid        NOT NULL REFERENCES gates (id) ON DELETE RESTRICT,
    gate_history_id   uuid        NOT NULL REFERENCES gate_history (id) ON DELETE RESTRICT,
    zone_id           uuid        NOT NULL REFERENCES zones (id) ON DELETE RESTRICT,
    scooter_id        uuid        NOT NULL REFERENCES scooters (id) ON DELETE RESTRICT,
    global_settings   uuid        NOT NULL REFERENCES zone_settings (id) ON DELETE RESTRICT,
    zone_settings     uuid        REFERENCES zone_settings (id) ON DELETE RESTRICT,
    state             text        NOT NULL CHECK (state IN ('PAYMENT_PENDING', 'PAYMENT_FAILED', 'PAYMENT_SECURED', 'OFFERED',
        'NO_SCOOTER', 'RIDER_ACCEPTED', 'PACKED', 'TIMER_RUNNING', 'ARRIVED', 'HANDED_OVER', 'COMPLETED', 'CANCELLED', 'TIMED_OUT', 'FAILED')),
    seq               bigint      NOT NULL DEFAULT 1 CHECK (seq >= 1),
    total_paise       integer     NOT NULL CHECK (total_paise > 0),
    currency          text        NOT NULL DEFAULT 'INR' CHECK (currency = 'INR'),
    hold_until        timestamptz NOT NULL,
    paid_at           timestamptz,
    cancel_until      timestamptz,
    wait_deadline     timestamptz,
    ended_reason      text CHECK (ended_reason IN ('cancelled', 'timed_out', 'failed')),
    ended_at          timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    -- One tap makes one order (data map section 7).
    UNIQUE (customer_id, idempotency_key),
    CHECK ((state IN ('CANCELLED', 'TIMED_OUT', 'FAILED')) = (ended_reason IS NOT NULL))
);
CREATE INDEX orders_customer_idx ON orders (customer_id, created_at DESC, id DESC);
CREATE INDEX orders_open_idx ON orders (state, hold_until) WHERE state IN ('PAYMENT_PENDING', 'PAYMENT_FAILED');
ALTER TABLE orders ENABLE ROW LEVEL SECURITY;

-- Items, with the price fixed at creation and the price_history row it came from.
CREATE TABLE order_items (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id         uuid    NOT NULL REFERENCES orders (id) ON DELETE RESTRICT,
    item_id          uuid    NOT NULL REFERENCES menu_items (id) ON DELETE RESTRICT,
    price_history_id uuid    NOT NULL REFERENCES price_history (id) ON DELETE RESTRICT,
    name             text    NOT NULL,
    qty              integer NOT NULL CHECK (qty BETWEEN 1 AND 20),
    unit_paise       integer NOT NULL CHECK (unit_paise > 0),
    UNIQUE (order_id, item_id)
);
ALTER TABLE order_items ENABLE ROW LEVEL SECURITY;

-- The true history of every order, never edited.
CREATE TABLE order_events (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    order_id   uuid        NOT NULL REFERENCES orders (id) ON DELETE RESTRICT,
    from_state text,
    to_state   text        NOT NULL,
    actor_type text        NOT NULL,
    actor_id   uuid,
    reason     text,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX order_events_order_idx ON order_events (order_id, id);
ALTER TABLE order_events ENABLE ROW LEVEL SECURITY;
CREATE TRIGGER order_events_append_only BEFORE UPDATE OR DELETE ON order_events
    FOR EACH ROW EXECUTE FUNCTION forbid_change();
CREATE TRIGGER order_events_no_truncate BEFORE TRUNCATE ON order_events
    FOR EACH STATEMENT EXECUTE FUNCTION forbid_change();

-- Units held for an order, lot by lot. held raises scooter_stock.reserved, released lowers it.
CREATE TABLE reservations (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id    uuid        NOT NULL REFERENCES orders (id) ON DELETE RESTRICT,
    scooter_id  uuid        NOT NULL REFERENCES scooters (id) ON DELETE RESTRICT,
    lot_id      uuid        NOT NULL REFERENCES lots (id) ON DELETE RESTRICT,
    item_id     uuid        NOT NULL REFERENCES menu_items (id) ON DELETE RESTRICT,
    qty         integer     NOT NULL CHECK (qty >= 1),
    status      text        NOT NULL DEFAULT 'held' CHECK (status IN ('held', 'released', 'handed_over')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    released_at timestamptz,
    UNIQUE (order_id, lot_id)
);
CREATE INDEX reservations_order_idx ON reservations (order_id);
ALTER TABLE reservations ENABLE ROW LEVEL SECURITY;

-- One row per payment attempt, so a retry is a new row.
CREATE TABLE payments (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id          uuid        NOT NULL REFERENCES orders (id) ON DELETE RESTRICT,
    provider          text        NOT NULL CHECK (provider IN ('test', 'razorpay')),
    provider_order_id text        NOT NULL UNIQUE,
    amount_paise      integer     NOT NULL CHECK (amount_paise > 0),
    status            text        NOT NULL DEFAULT 'created' CHECK (status IN ('created', 'paid', 'failed', 'refunded')),
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX payments_order_idx ON payments (order_id, created_at DESC);
-- At most one paid attempt per order.
CREATE UNIQUE INDEX payments_one_paid ON payments (order_id) WHERE status = 'paid';
ALTER TABLE payments ENABLE ROW LEVEL SECURITY;

-- Refunds. Unique per payment and reason, and never more than was paid (data map section 7).
CREATE TABLE refunds (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id     uuid        NOT NULL REFERENCES orders (id) ON DELETE RESTRICT,
    payment_id   uuid        NOT NULL REFERENCES payments (id) ON DELETE RESTRICT,
    reason_code  text        NOT NULL,
    method       text        NOT NULL DEFAULT 'original_payment_method',
    status       text        NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'processed', 'failed')),
    amount_paise integer     NOT NULL CHECK (amount_paise > 0),
    expected_by  timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (payment_id, reason_code)
);
ALTER TABLE refunds ENABLE ROW LEVEL SECURITY;
-- +goose StatementBegin
CREATE FUNCTION refunds_within_payment() RETURNS trigger
LANGUAGE plpgsql
SET search_path = ''
AS $$
BEGIN
    PERFORM 1 FROM public.payments WHERE id = NEW.payment_id FOR UPDATE;
    IF (SELECT coalesce(sum(amount_paise), 0) FROM public.refunds WHERE payment_id = NEW.payment_id AND status <> 'failed')
        > (SELECT amount_paise FROM public.payments WHERE id = NEW.payment_id) THEN
        RAISE EXCEPTION 'refunds would exceed the payment';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION refunds_within_payment() FROM PUBLIC;
CREATE CONSTRAINT TRIGGER refunds_within_payment AFTER INSERT OR UPDATE ON refunds
    DEFERRABLE INITIALLY IMMEDIATE FOR EACH ROW EXECUTE FUNCTION refunds_within_payment();

-- +goose Down
DROP TABLE refunds;
DROP FUNCTION refunds_within_payment();
DROP TABLE payments;
DROP TABLE reservations;
DROP TABLE order_events;
DROP TABLE order_items;
DROP TABLE orders;
ALTER TABLE zone_settings DROP COLUMN payment_hold_s;
