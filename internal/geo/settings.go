// Package geo holds zones, gates and the settings that shape an order.
package geo

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
)

// Setting describes one value the owner may change. The console will use this same list.
type Setting struct {
	Name     string // As written in a change, and the database column.
	Unit     string
	Min, Max int
	Required bool // An order cannot run without it, so the global row must hold it.
	About    string
}

// Settings lists every setting, in one place. Adding one means a migration column and a line here.
var Settings = []Setting{
	{"timer_length_s", "seconds", 30, 600, true, "How long the visible timer runs"},
	{"zone_radius_m", "metres", 50, 5000, true, "The zone around the customer where the timer can start"},
	{"arrival_perimeter_m", "metres", 10, 1000, true, "Entering this stops the timer"},
	{"min_search_wait_s", "seconds", 0, 600, false, "Shortest 'looking for riders' wait"},
	{"in_zone_wait_s", "seconds", 1, 600, false, "The shorter wait when a rider is already in the zone, never zero"},
	{"packing_limit_s", "seconds", 10, 900, false, "When an order counts as packed with no rider action"},
	{"max_wait_before_timer_s", "seconds", 30, 3600, true, "Longest wait before the timer starts"},
	{"no_scooter_choice_s", "seconds", 10, 900, false, "Time to choose after no scooter, before an automatic refund"},
	{"cancel_window_s", "seconds", 0, 60, true, "Free cancel after payment is confirmed"},
	{"remedy_credit_paise", "paise", 0, 100000, false, "Credit when the timer ends before the perimeter"},
	{"signal_release_s", "seconds", 5, 600, true, "No ping for this long releases the handover code"},
	{"reassign_after_s", "seconds", 30, 1800, true, "No ping for this long reassigns the order"},
	{"sweet_spot_budget_s", "seconds", 10, 600, true, "Travel time budget for a scooter to serve a gate"},
	{"straight_line_speed_kmh", "km/h", 3, 60, true, "Speed used for straight-line reach, until real road times"},
	{"max_search_radius_m", "metres", 100, 20000, true, "Widest rider search. Past it, the order is refused before payment"},
	{"payment_hold_s", "seconds", 60, 3600, true, "How long an unpaid order holds its stock"},
	{"offer_timeout_s", "seconds", 10, 300, true, "How long a rider has to answer an offer"},
	{"restock_low_qty", "units", 0, 500, true, "Fewer free units of an item than this drafts a restock suggestion"},
	{"restock_target_qty", "units", 1, 500, true, "A restock suggestion brings an item back up to this"},
	{"restock_suggestion_ttl_s", "seconds", 60, 3600, true, "How long a restock suggestion stays open"},
	{"restock_check_s", "seconds", 60, 3600, true, "The wait before a scooter is suggested a restock again, after one ends"},
	{"restock_accept_s", "seconds", 120, 7200, true, "A confirmed restock no hub accepts within this time expires, and the rider is told"},
	{"relocation_window_s", "seconds", 300, 7200, true, "How far back orders count as demand at a node"},
	{"relocation_min_orders", "orders", 1, 50, true, "Orders in the window that make a node with nobody waiting ask for a rider"},
	{"relocation_ttl_s", "seconds", 60, 1800, true, "How long a suggested move stays open"},
	{"relocation_rest_s", "seconds", 60, 7200, true, "The wait before a rider is asked to move again, after a move ends"},
	{"relocation_arrive_m", "metres", 20, 500, true, "How close to the new node a ping marks the rider arrived"},
	{"pay_per_order_paise", "paise", 0, 100000, true, "What a rider earns for each order handed over"},
	{"incentive_orders", "orders", 1, 200, true, "Orders handed over in a day that earn the day's incentive"},
	{"incentive_paise", "paise", 0, 1000000, true, "The day's incentive"},
	{"relocation_paise_per_minute", "paise", 0, 10000, true, "Pay for each started minute of an accepted move"},
	{"relocation_max_paid_s", "seconds", 60, 3600, true, "The most moving time paid for one move"},
}

// ErrInvalidSetting means a change was refused, and nothing was stored.
var ErrInvalidSetting = errors.New("geo: invalid setting")

func find(name string) (Setting, bool) {
	for _, s := range Settings {
		if s.Name == name {
			return s, true
		}
	}
	return Setting{}, false
}

// Effective is the settings one zone runs with: its own values, then the global ones.
type Effective struct {
	Values        map[string]*int // Nil means not set.
	GlobalVersion int
	ZoneVersion   int    // 0 when the zone overrides nothing.
	GlobalRowID   string // An order keeps both row ids, so its values never change under it.
	ZoneRowID     string // Empty when the zone overrides nothing.
}

// Get returns a value, and whether it is set.
func (e Effective) Get(name string) (int, bool) {
	v := e.Values[name]
	if v == nil {
		return 0, false
	}
	return *v, true
}

type querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func columns() string {
	names := make([]string, len(Settings))
	for i, s := range Settings {
		names[i] = s.Name
	}
	return strings.Join(names, ", ")
}

// latest reads the newest version for a zone, or the global row when zoneID is empty.
func latest(ctx context.Context, q querier, zoneID string) (map[string]*int, int, error) {
	values := make([]*int, len(Settings))
	dest := []any{new(int)}
	for i := range values {
		dest = append(dest, &values[i])
	}
	query := "SELECT version, " + columns() + " FROM zone_settings WHERE zone_id IS NULL ORDER BY version DESC LIMIT 1"
	args := []any{}
	if zoneID != "" {
		query = "SELECT version, " + columns() + " FROM zone_settings WHERE zone_id = $1 ORDER BY version DESC LIMIT 1"
		args = append(args, zoneID)
	}
	err := q.QueryRow(ctx, query, args...).Scan(dest...)
	if errors.Is(err, pgx.ErrNoRows) {
		return map[string]*int{}, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	out := map[string]*int{}
	for i, s := range Settings {
		out[s.Name] = values[i]
	}
	return out, *dest[0].(*int), nil
}

// OwnSettings returns one level's own values and version: the global row when zoneID is empty, or
// what a zone overrides (version 0 when it has none yet).
func OwnSettings(ctx context.Context, q querier, zoneID string) (map[string]*int, int, error) {
	return latest(ctx, q, zoneID)
}

// LoadSettings returns what a zone runs with now. An empty zoneID gives the global values. Both rows
// come from one statement, so they are always from the same moment.
func LoadSettings(ctx context.Context, q querier, zoneID string) (Effective, error) {
	n := len(Settings)
	global, own := make([]*int, n), make([]*int, n)
	var gid, zid *string
	var gv, zv *int
	dest := []any{&gid, &gv, &zid, &zv}
	for i := range global {
		dest = append(dest, &global[i])
	}
	for i := range own {
		dest = append(dest, &own[i])
	}
	g, z := make([]string, n), make([]string, n)
	for i, st := range Settings {
		g[i], z[i] = "g."+st.Name, "z."+st.Name
	}
	err := q.QueryRow(ctx, `SELECT g.id::text, g.version, z.id::text, z.version, `+strings.Join(g, ", ")+`, `+strings.Join(z, ", ")+`
		FROM (SELECT * FROM zone_settings WHERE zone_id IS NULL ORDER BY version DESC LIMIT 1) g
		LEFT JOIN (SELECT * FROM zone_settings WHERE zone_id = NULLIF($1, '')::uuid ORDER BY version DESC LIMIT 1) z ON true`,
		zoneID).Scan(dest...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Effective{}, errors.New("geo: no global settings, so migration 0012 has not run")
	}
	if err != nil {
		return Effective{}, err
	}
	e := Effective{Values: map[string]*int{}, GlobalVersion: *gv, GlobalRowID: *gid}
	if zv != nil {
		e.ZoneVersion, e.ZoneRowID = *zv, *zid
	}
	for i, st := range Settings {
		e.Values[st.Name] = global[i]
		if own[i] != nil {
			e.Values[st.Name] = own[i]
		}
	}
	return e, nil
}

// Change is one requested change. A nil Value clears a zone's override.
type Change struct {
	Name  string
	Value *int
}

// ParseChanges reads "name=value" pairs. "name=" clears a zone's override.
func ParseChanges(args []string) ([]Change, error) {
	var out []Change
	seen := map[string]bool{}
	for _, arg := range args {
		name, raw, ok := strings.Cut(arg, "=")
		s, known := find(name)
		if !ok || !known {
			return nil, fmt.Errorf("%w: %q is not name=value with a known name", ErrInvalidSetting, arg)
		}
		if seen[name] {
			return nil, fmt.Errorf("%w: %s is given twice", ErrInvalidSetting, name)
		}
		seen[name] = true
		if raw == "" {
			out = append(out, Change{Name: name})
			continue
		}
		v, err := strconv.Atoi(raw)
		if err != nil || v < s.Min || v > s.Max {
			return nil, fmt.Errorf("%w: %s must be a whole number from %d to %d %s", ErrInvalidSetting, name, s.Min, s.Max, s.Unit)
		}
		out = append(out, Change{Name: name, Value: &v})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: no changes given", ErrInvalidSetting)
	}
	return out, nil
}

// ChangeSettings stores a new version for a zone, or for the global row when zoneID is empty, and
// records it. The result must still make sense in every zone, or nothing is stored.
func ChangeSettings(ctx context.Context, tx pgx.Tx, zoneID string, changes []Change, reason, by string) (int, error) {
	reason, by = strings.TrimSpace(reason), strings.TrimSpace(by)
	if len(reason) < 3 {
		return 0, fmt.Errorf("%w: say why, in at least 3 characters", ErrInvalidSetting)
	}
	if by == "" {
		return 0, fmt.Errorf("%w: say who is making the change", ErrInvalidSetting)
	}
	// One change at a time, everywhere. A global change and a zone change can contradict each other,
	// and each check would miss the other's row. Changes are rare, so nobody waits long.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended('zone_settings', 5))"); err != nil {
		return 0, err
	}
	if zoneID != "" {
		var exists bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM zones WHERE id::text = $1)", zoneID).Scan(&exists); err != nil {
			return 0, err
		}
		if !exists {
			return 0, fmt.Errorf("%w: no zone %s", ErrInvalidSetting, zoneID)
		}
	}
	current, version, err := latest(ctx, tx, zoneID)
	if err != nil {
		return 0, err
	}
	next := map[string]*int{}
	for name, v := range current {
		next[name] = v
	}
	before := map[string]any{}
	after := map[string]any{}
	for _, c := range changes {
		s, _ := find(c.Name)
		if c.Value == nil && (zoneID == "" && s.Required) {
			return 0, fmt.Errorf("%w: %s cannot be cleared globally, because orders need it", ErrInvalidSetting, c.Name)
		}
		before[c.Name] = current[c.Name]
		after[c.Name] = c.Value
		next[c.Name] = c.Value
	}
	cols := []string{"zone_id", "version", "reason", "created_by"}
	args := []any{nil, version + 1, reason, by}
	if zoneID != "" {
		args[0] = zoneID
	}
	for _, s := range Settings {
		cols = append(cols, s.Name)
		args = append(args, next[s.Name])
	}
	marks := make([]string, len(cols))
	for i := range marks {
		marks[i] = "$" + strconv.Itoa(i+1)
	}
	var id string
	err = tx.QueryRow(ctx, "INSERT INTO zone_settings ("+strings.Join(cols, ", ")+") VALUES ("+strings.Join(marks, ", ")+") RETURNING id::text", args...).Scan(&id)
	if err != nil {
		return 0, err
	}
	if err := checkAllZones(ctx, tx); err != nil {
		return 0, err
	}
	return version + 1, audit.Record(ctx, tx, audit.Event{
		ActorType: audit.ActorSystem, Action: "settings.changed", EntityType: "zone_settings", EntityID: id,
		Before: before, After: after, Reason: by + ": " + reason,
	})
}

// checkAllZones refuses a change that leaves any zone with rules that contradict each other. A
// global change can break a zone that overrides something else.
func checkAllZones(ctx context.Context, tx pgx.Tx) error {
	rows, err := tx.Query(ctx, "SELECT id::text FROM zones")
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, id := range append([]string{""}, ids...) {
		e, err := LoadSettings(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := e.check(); err != nil {
			where := "globally"
			if id != "" {
				where = "in zone " + id
			}
			return fmt.Errorf("%w: %s %s", ErrInvalidSetting, err.Error(), where)
		}
	}
	return nil
}

func (e Effective) check() error {
	perimeter, _ := e.Get("arrival_perimeter_m")
	radius, _ := e.Get("zone_radius_m")
	if perimeter >= radius {
		return fmt.Errorf("the arrival perimeter (%d m) must be inside the zone radius (%d m)", perimeter, radius)
	}
	if search, _ := e.Get("max_search_radius_m"); search < radius {
		return fmt.Errorf("the widest search (%d m) must reach at least the zone radius (%d m)", search, radius)
	}
	if inZone, ok := e.Get("in_zone_wait_s"); ok {
		if search, ok := e.Get("min_search_wait_s"); ok && inZone > search {
			return fmt.Errorf("the wait for a rider already in the zone (%d s) must not be longer than the usual wait (%d s)", inZone, search)
		}
	}
	signal, _ := e.Get("signal_release_s")
	reassign, _ := e.Get("reassign_after_s")
	if reassign <= signal {
		return fmt.Errorf("reassigning (%d s) must come after releasing the code (%d s)", reassign, signal)
	}
	return nil
}

// Describe lists the settings and their values, for people to read.
func (e Effective) Describe() string {
	var b strings.Builder
	names := make([]string, 0, len(Settings))
	for _, s := range Settings {
		names = append(names, s.Name)
	}
	sort.Strings(names)
	for _, name := range names {
		s, _ := find(name)
		value := "not set"
		if v, ok := e.Get(name); ok {
			value = fmt.Sprintf("%d %s", v, s.Unit)
		}
		fmt.Fprintf(&b, "%-24s %-16s %s\n", name, value, s.About)
	}
	return b.String()
}
