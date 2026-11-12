-- +goose Up
-- One queue for what needs a person (issue 148, decision 0015): support alerts, problem reports,
-- lost signals, open arrival reviews and open tickets, each with an assignee, notes and a status.
--
-- Incidents are opened by triggers on the source tables, in the same transaction as the source
-- row, so a source row can neither be missed nor doubled, whichever code wrote it. The same
-- statements backfill the rows that exist now.

-- Take every lock first, in one fixed order, as 0024 and 0042 do: the background pass and the
-- APIs write to these tables every few seconds, and doing the steps below one after another can
-- deadlock with a transaction that holds one and wants another. The referenced tables only need
-- to stop changing (the lock a foreign key takes); the altered ones are locked fully.
LOCK TABLE orders, riders, console_users IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE problem_reports, support_tickets, arrival_reviews, outbox_events IN ACCESS EXCLUSIVE MODE;

-- A problem report has no state of its own. Closing its incident marks it handled.
ALTER TABLE problem_reports ADD COLUMN handled_at timestamptz;

CREATE TABLE incidents (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind         text        NOT NULL CHECK (kind IN ('support_alert', 'problem_report', 'arrival_review', 'ticket', 'signal_lost')),
    -- The alert (an outbox event id), report, review or ticket this came from. No foreign key:
    -- a handled alert is purged from the outbox after a week and the incident stays.
    source_id    text        NOT NULL CHECK (char_length(source_id) BETWEEN 1 AND 64),
    order_id     uuid        REFERENCES orders (id) ON DELETE RESTRICT,
    rider_id     uuid        REFERENCES riders (id) ON DELETE RESTRICT,
    status       text        NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'assigned', 'closed')),
    assigned_to  uuid        REFERENCES console_users (id) ON DELETE RESTRICT,
    opened_at    timestamptz NOT NULL DEFAULT now(),
    closed_at    timestamptz,
    -- Null when the incident closed by itself because its ticket or review closed.
    closed_by    uuid        REFERENCES console_users (id) ON DELETE RESTRICT,
    close_reason text        CHECK (close_reason IS NULL OR char_length(close_reason) BETWEEN 3 AND 500),
    CONSTRAINT incidents_closed_together CHECK ((status = 'closed') = (closed_at IS NOT NULL)),
    CONSTRAINT incidents_assigned_together CHECK (status <> 'assigned' OR assigned_to IS NOT NULL),
    CONSTRAINT incidents_open_unassigned CHECK (status <> 'open' OR assigned_to IS NULL),
    CONSTRAINT incidents_close_details CHECK ((closed_by IS NULL AND close_reason IS NULL) OR status = 'closed')
);
-- One incident per source row.
CREATE UNIQUE INDEX incidents_one_per_source ON incidents (kind, source_id);
-- A lost signal's report and alert are one incident, and a later report for the same order joins
-- the one still open. Once it is closed, a new lost signal opens a new one.
CREATE UNIQUE INDEX incidents_one_open_signal_lost ON incidents (order_id) WHERE kind = 'signal_lost' AND status <> 'closed';
CREATE INDEX incidents_newest_idx ON incidents (opened_at DESC, id DESC);
CREATE INDEX incidents_status_idx ON incidents (status, opened_at DESC, id DESC);
CREATE INDEX incidents_kind_idx ON incidents (kind, opened_at DESC, id DESC);
ALTER TABLE incidents ENABLE ROW LEVEL SECURITY;

CREATE TABLE incident_notes (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    incident_id uuid        NOT NULL REFERENCES incidents (id) ON DELETE RESTRICT,
    author_id   uuid        NOT NULL REFERENCES console_users (id) ON DELETE RESTRICT,
    text        text        NOT NULL CHECK (char_length(text) BETWEEN 1 AND 2000),
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX incident_notes_incident_idx ON incident_notes (incident_id, created_at, id);
ALTER TABLE incident_notes ENABLE ROW LEVEL SECURITY;

-- A uuid from text that may not be one (an alert's payload is whatever the app sent), or null.
-- +goose StatementBegin
CREATE FUNCTION incident_uuid(value text) RETURNS uuid
LANGUAGE sql IMMUTABLE
SET search_path = ''
AS $$
    SELECT CASE WHEN value ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' THEN value::uuid END
$$;
-- +goose StatementEnd

-- Opens an incident unless one already stands for this source (or, for a lost signal, this order).
-- An order or rider that does not exist is stored as null, so a stray id in an alert can never
-- stop the source row from being written.
-- +goose StatementBegin
CREATE FUNCTION incident_open(p_kind text, p_source text, p_order uuid, p_rider uuid, p_opened timestamptz) RETURNS void
LANGUAGE sql
SET search_path = ''
AS $$
    INSERT INTO public.incidents (kind, source_id, order_id, rider_id, opened_at)
    VALUES (p_kind, p_source, (SELECT id FROM public.orders WHERE id = p_order), (SELECT id FROM public.riders WHERE id = p_rider), p_opened)
    ON CONFLICT DO NOTHING
$$;
-- +goose StatementEnd

-- Closes the incident of a source that closed through its own call (or timed out).
-- +goose StatementBegin
CREATE FUNCTION incident_source_closed(p_kind text, p_source text) RETURNS void
LANGUAGE sql
SET search_path = ''
AS $$
    UPDATE public.incidents SET status = 'closed', closed_at = now(), close_reason = 'Closed with its source'
    WHERE kind = p_kind AND source_id = p_source AND status <> 'closed'
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION incident_from_ticket() RETURNS trigger
LANGUAGE plpgsql
SET search_path = ''
AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.status = 'open' THEN
            PERFORM public.incident_open('ticket', NEW.id::text, NEW.order_id, NULL, NEW.created_at);
        END IF;
    ELSIF OLD.status = 'open' AND NEW.status <> 'open' THEN
        PERFORM public.incident_source_closed('ticket', NEW.id::text);
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION incident_from_review() RETURNS trigger
LANGUAGE plpgsql
SET search_path = ''
AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.status = 'open' THEN
            PERFORM public.incident_open('arrival_review', NEW.id::text, NEW.order_id, (SELECT rider_id FROM public.orders WHERE id = NEW.order_id), NEW.created_at);
        END IF;
    ELSIF OLD.status = 'open' AND NEW.status <> 'open' THEN
        PERFORM public.incident_source_closed('arrival_review', NEW.id::text);
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION incident_from_problem() RETURNS trigger
LANGUAGE plpgsql
SET search_path = ''
AS $$
BEGIN
    PERFORM public.incident_open(CASE WHEN NEW.type = 'signal_lost' THEN 'signal_lost' ELSE 'problem_report' END, NEW.id::text, NEW.order_id, NEW.rider_id, NEW.created_at);
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- A lost signal also raises a support alert; its problem report already makes the signal_lost
-- incident, so that alert does not make a second one.
-- +goose StatementBegin
CREATE FUNCTION incident_from_alert() RETURNS trigger
LANGUAGE plpgsql
SET search_path = ''
AS $$
DECLARE
    alert_order uuid := public.incident_uuid(NEW.payload->>'order_id');
BEGIN
    IF NEW.payload->>'reason' = 'signal_lost' AND EXISTS (
        SELECT 1 FROM public.problem_reports WHERE order_id = alert_order AND type = 'signal_lost' AND source = 'system') THEN
        RETURN NULL;
    END IF;
    PERFORM public.incident_open('support_alert', NEW.id::text, alert_order, public.incident_uuid(NEW.payload->>'rider_id'), NEW.created_at);
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- The trigger functions are only ever run by their triggers. The helpers they call run as the
-- service login, so it may execute them; nobody else may, including Supabase's API roles.
REVOKE ALL ON FUNCTION incident_uuid(text), incident_open(text, text, uuid, uuid, timestamptz), incident_source_closed(text, text),
    incident_from_ticket(), incident_from_review(), incident_from_problem(), incident_from_alert() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION incident_uuid(text), incident_open(text, text, uuid, uuid, timestamptz), incident_source_closed(text, text) TO ninzo_service;
-- +goose StatementBegin
DO $$
DECLARE
    api_role text;
BEGIN
    FOREACH api_role IN ARRAY ARRAY['anon', 'authenticated', 'service_role'] LOOP
        IF EXISTS (SELECT FROM pg_roles WHERE rolname = api_role) THEN
            EXECUTE format('REVOKE ALL ON FUNCTION incident_uuid(text), incident_open(text, text, uuid, uuid, timestamptz), incident_source_closed(text, text), '
                'incident_from_ticket(), incident_from_review(), incident_from_problem(), incident_from_alert() FROM %I', api_role);
        END IF;
    END LOOP;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER incident_from_ticket AFTER INSERT OR UPDATE OF status ON support_tickets
    FOR EACH ROW EXECUTE FUNCTION incident_from_ticket();
CREATE TRIGGER incident_from_review AFTER INSERT OR UPDATE OF status ON arrival_reviews
    FOR EACH ROW EXECUTE FUNCTION incident_from_review();
CREATE TRIGGER incident_from_problem AFTER INSERT ON problem_reports
    FOR EACH ROW EXECUTE FUNCTION incident_from_problem();
CREATE TRIGGER incident_from_alert AFTER INSERT ON outbox_events
    FOR EACH ROW WHEN (NEW.topic = 'support.alert') EXECUTE FUNCTION incident_from_alert();

-- Before 0044 nothing marked a problem report or a support alert handled, so every one ever made
-- would look open. Those whose order has ended, or that have no order and are over a day old, are
-- settled here, so the backfill opens incidents only for what may still need a person (#148 review).
-- Settled alerts also leave the outbox's due index.
UPDATE problem_reports p SET handled_at = now()
WHERE p.handled_at IS NULL AND (
    EXISTS (SELECT 1 FROM orders o WHERE o.id = p.order_id AND o.ended_at IS NOT NULL)
    OR (p.order_id IS NULL AND p.created_at < now() - interval '1 day'));
UPDATE outbox_events e SET delivered_at = now()
WHERE e.topic = 'support.alert' AND e.delivered_at IS NULL AND (
    EXISTS (SELECT 1 FROM orders o WHERE o.id = incident_uuid(e.payload->>'order_id') AND o.ended_at IS NOT NULL)
    OR (incident_uuid(e.payload->>'order_id') IS NULL AND e.created_at < now() - interval '1 day'));

-- Closing a lost signal marks every open alert of its order handled; this keeps that lookup small.
CREATE INDEX outbox_events_open_alert_order_idx ON outbox_events ((payload->>'order_id'))
    WHERE topic = 'support.alert' AND delivered_at IS NULL;

-- Backfill: everything that needs a person now. Lost signals go first, so an alert for the same
-- order is not counted twice, and the oldest lost signal of an order is the one kept, as the
-- trigger would have kept the first. The unique indexes make each statement safe to run twice.
INSERT INTO incidents (kind, source_id, order_id, rider_id, opened_at)
SELECT 'signal_lost', id::text, order_id, rider_id, created_at FROM problem_reports
WHERE type = 'signal_lost' AND handled_at IS NULL ORDER BY created_at, id
ON CONFLICT DO NOTHING;
INSERT INTO incidents (kind, source_id, order_id, rider_id, opened_at)
SELECT 'problem_report', id::text, order_id, rider_id, created_at FROM problem_reports
WHERE type <> 'signal_lost' AND handled_at IS NULL
ON CONFLICT DO NOTHING;
INSERT INTO incidents (kind, source_id, order_id, rider_id, opened_at)
SELECT 'arrival_review', r.id::text, r.order_id, o.rider_id, r.created_at FROM arrival_reviews r JOIN orders o ON o.id = r.order_id
WHERE r.status = 'open'
ON CONFLICT DO NOTHING;
INSERT INTO incidents (kind, source_id, order_id, rider_id, opened_at)
SELECT 'ticket', id::text, order_id, NULL, created_at FROM support_tickets WHERE status = 'open'
ON CONFLICT DO NOTHING;
INSERT INTO incidents (kind, source_id, order_id, rider_id, opened_at)
SELECT 'support_alert', e.id::text, o.id, r.id, e.created_at FROM outbox_events e
LEFT JOIN orders o ON o.id = incident_uuid(e.payload->>'order_id')
LEFT JOIN riders r ON r.id = incident_uuid(e.payload->>'rider_id')
WHERE e.topic = 'support.alert' AND e.delivered_at IS NULL
    AND NOT (e.payload->>'reason' = 'signal_lost' AND EXISTS (
        SELECT 1 FROM problem_reports p WHERE p.order_id = incident_uuid(e.payload->>'order_id') AND p.type = 'signal_lost' AND p.source = 'system'))
ON CONFLICT DO NOTHING;

-- +goose Down
LOCK TABLE orders, riders, console_users IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE problem_reports, support_tickets, arrival_reviews, outbox_events IN ACCESS EXCLUSIVE MODE;
DROP INDEX outbox_events_open_alert_order_idx;
DROP TRIGGER incident_from_alert ON outbox_events;
DROP TRIGGER incident_from_problem ON problem_reports;
DROP TRIGGER incident_from_review ON arrival_reviews;
DROP TRIGGER incident_from_ticket ON support_tickets;
DROP FUNCTION incident_from_alert();
DROP FUNCTION incident_from_problem();
DROP FUNCTION incident_from_review();
DROP FUNCTION incident_from_ticket();
DROP FUNCTION incident_source_closed(text, text);
DROP FUNCTION incident_open(text, text, uuid, uuid, timestamptz);
DROP FUNCTION incident_uuid(text);
DROP TABLE incident_notes;
DROP TABLE incidents;
ALTER TABLE problem_reports DROP COLUMN handled_at;
