// Package roaddata records which OSRM road data build is in use (issue 190, geo record W1 step 6),
// so every stored road time can be traced to the map, profile and engine that made it.
package roaddata

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Version is one build, as tools/osrm/build.sh writes it to build.json.
type Version struct {
	Name             string    `json:"name"`
	Source           string    `json:"source"`
	SourceSHA256     string    `json:"source_sha256"`
	ClipBox          string    `json:"clip_box"`
	OSMExtractSHA256 string    `json:"osm_extract_sha256"`
	OSMTimestamp     time.Time `json:"osm_timestamp"`
	Profile          string    `json:"profile"`
	ProfileVersion   string    `json:"profile_version"`
	ProfileSHA256    string    `json:"profile_sha256"`
	OSRMImage        string    `json:"osrm_image"`
	Algorithm        string    `json:"algorithm"`
	BuiltAt          time.Time `json:"built_at"`
}

var (
	nameRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	shaRe     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	profileRe = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)
	pvRe      = regexp.MustCompile(`^[a-z0-9_.-]{1,40}$`)
	// boxRe is the table's own pattern, so a box Go accepts is a box the table accepts.
	boxRe = regexp.MustCompile(`^-?[0-9.]+,-?[0-9.]+,-?[0-9.]+,-?[0-9.]+$`)
)

// Parse reads a build.json and checks every field, so a partial or hand-edited file is refused
// before anything is stored. Unknown fields are allowed (the build adds things such as its time).
func Parse(data []byte) (Version, error) {
	// The times are read as text first, so an empty or bad one is reported by its field name.
	type plain Version
	var raw struct {
		plain
		OSMTimestamp string `json:"osm_timestamp"`
		BuiltAt      string `json:"built_at"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&raw); err != nil {
		return Version{}, fmt.Errorf("build.json: %w", err)
	}
	if dec.More() {
		return Version{}, errors.New("build.json: more than one JSON value")
	}
	v := Version(raw.plain)
	// A bad time stays zero, and Check names it.
	v.OSMTimestamp, _ = time.Parse(time.RFC3339, raw.OSMTimestamp)
	v.BuiltAt, _ = time.Parse(time.RFC3339, raw.BuiltAt)
	return v, v.Check()
}

// Check says what is wrong with a Version, if anything.
func (v Version) Check() error {
	var bad []string
	if !nameRe.MatchString(v.Name) {
		bad = append(bad, "name")
	}
	if v.Source == "" || len(v.Source) > 500 {
		bad = append(bad, "source")
	}
	for field, s := range map[string]string{"source_sha256": v.SourceSHA256, "osm_extract_sha256": v.OSMExtractSHA256, "profile_sha256": v.ProfileSHA256} {
		if !shaRe.MatchString(s) {
			bad = append(bad, field)
		}
	}
	if !validBox(v.ClipBox) {
		bad = append(bad, "clip_box")
	}
	if v.OSMTimestamp.IsZero() {
		bad = append(bad, "osm_timestamp")
	}
	if !profileRe.MatchString(v.Profile) {
		bad = append(bad, "profile")
	}
	if !pvRe.MatchString(v.ProfileVersion) {
		bad = append(bad, "profile_version")
	}
	if v.OSRMImage == "" || len(v.OSRMImage) > 300 {
		bad = append(bad, "osrm_image")
	}
	if v.Algorithm != "mld" && v.Algorithm != "ch" {
		bad = append(bad, "algorithm")
	}
	if v.BuiltAt.IsZero() {
		bad = append(bad, "built_at")
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return fmt.Errorf("build.json: missing or bad %s", strings.Join(bad, ", "))
	}
	if v.OSMTimestamp.After(v.BuiltAt) {
		return errors.New("build.json: the map is newer than the build")
	}
	return nil
}

// validBox is lng_min,lat_min,lng_max,lat_max with min below max, in range.
func validBox(s string) bool {
	parts := strings.Split(s, ",")
	if !boxRe.MatchString(s) || len(parts) != 4 {
		return false
	}
	var f [4]float64
	for i, p := range parts {
		v, err := strconv.ParseFloat(p, 64)
		if err != nil {
			return false
		}
		f[i] = v
	}
	return f[0] >= -180 && f[2] <= 180 && f[1] >= -90 && f[3] <= 90 && f[0] < f[2] && f[1] < f[3]
}

// Register stores a build once. The same build (same extract, profile, image and algorithm)
// answers the id it already has, with created false; its stored fields are never changed.
func Register(ctx context.Context, tx pgx.Tx, v Version) (id string, created bool, err error) {
	if err := v.Check(); err != nil {
		return "", false, err
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO road_data_versions (name, source, source_sha256, clip_box, osm_extract_sha256, osm_timestamp,
			profile, profile_version, profile_sha256, osrm_image, algorithm, built_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT ON CONSTRAINT road_data_versions_one_build DO NOTHING
		RETURNING id::text`,
		v.Name, v.Source, v.SourceSHA256, v.ClipBox, v.OSMExtractSHA256, v.OSMTimestamp.UTC(),
		v.Profile, v.ProfileVersion, v.ProfileSHA256, v.OSRMImage, v.Algorithm, v.BuiltAt.UTC()).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", false, err
	}
	err = tx.QueryRow(ctx, `SELECT id::text FROM road_data_versions
		WHERE osm_extract_sha256 = $1 AND profile_sha256 = $2 AND osrm_image = $3 AND algorithm = $4`,
		v.OSMExtractSHA256, v.ProfileSHA256, v.OSRMImage, v.Algorithm).Scan(&id)
	return id, false, err
}

// Row is a stored build, for listing.
type Row struct {
	ID string
	Version
	RegisteredAt time.Time
}

// List returns the stored builds, newest first.
func List(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) ([]Row, error) {
	rows, err := q.Query(ctx, `SELECT id::text, name, source, source_sha256, clip_box, osm_extract_sha256, osm_timestamp,
		profile, profile_version, profile_sha256, osrm_image, algorithm, built_at, registered_at
		FROM road_data_versions ORDER BY registered_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.ID, &r.Name, &r.Source, &r.SourceSHA256, &r.ClipBox, &r.OSMExtractSHA256, &r.OSMTimestamp,
			&r.Profile, &r.ProfileVersion, &r.ProfileSHA256, &r.OSRMImage, &r.Algorithm, &r.BuiltAt, &r.RegisteredAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
