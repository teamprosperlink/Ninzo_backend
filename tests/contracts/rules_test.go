// Package contracts checks the written rules in contracts/README.md that a
// linter cannot check. Every OpenAPI file under contracts/ except shared is covered.
package contracts

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const contractsDir = "../../contracts"

// operationsWithoutIdempotency are the only writes allowed to skip Idempotency-Key.
var operationsWithoutIdempotency = map[string]bool{
	"requestOtp":         true,
	"verifyOtp":          true,
	"signInWithGoogle":   true,
	"requestPhoneOtp":    true,
	"verifyPhone":        true,
	"changeSignInMethod": true,
	"confirmTestPayment": true,
	"signOut":            true,
	"refreshToken":       true,
	"razorpayWebhook":    true,
	"locationPing":       true,
	"sendEventBatch":     true,
	"consoleLogin":       true,
	"enrollTotp":         true,
	"verifyTotp":         true,
}

// operationsWithoutBearer are the only operations allowed to skip sign-in.
var operationsWithoutBearer = map[string]bool{
	"requestOtp":       true,
	"verifyOtp":        true,
	"signInWithGoogle": true,
	"refreshToken":     true,
	"razorpayWebhook":  true,
	"consoleLogin":     true,
	"enrollTotp":       true,
	"verifyTotp":       true,
}

// hiddenCustomerFields would reveal that a rider is allotted during the wait.
var hiddenCustomerFields = map[string]bool{
	"state":             true,
	"order_state":       true,
	"rider_accepted_at": true,
	"rider_id":          true,
	"packed_at":         true,
	"accepted_at":       true,
	"scooter_id":        true,
	"offer_id":          true,
}

var hiddenCustomerRefs = regexp.MustCompile(`#/components/schemas/(OrderState|OrderInternalTimes)$`)

// hiddenRiderFields would put a countdown or a customer's contact details in front of the rider.
var hiddenRiderFields = map[string]bool{
	"timer_started_at": true,
	"timer_duration_s": true,
	"timer_stopped_at": true,
	"timer_missed":     true,
	"timer":            true,
	"seconds_left":     true,
	"customer_phone":   true,
	"customer_id":      true,
	"handover_otp":     true,
	"customer_stage":   true,
}

var hiddenRiderRefs = regexp.MustCompile(`#/components/schemas/(OrderTimer|CustomerStage)$`)

type doc struct {
	path string
	root map[string]any
}

type loader struct{ docs map[string]*doc }

func (l *loader) load(path string) (*doc, error) {
	path = filepath.Clean(path)
	if d, ok := l.docs[path]; ok {
		return d, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var root map[string]any
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	d := &doc{path: path, root: root}
	l.docs[path] = d
	return d, nil
}

// resolve follows a $ref chain and returns the target with the document it lives in.
func (l *loader) resolve(d *doc, node map[string]any) (*doc, map[string]any, error) {
	for depth := 0; depth < 20; depth++ {
		ref, ok := node["$ref"].(string)
		if !ok {
			return d, node, nil
		}
		file, pointer, _ := strings.Cut(ref, "#")
		target := d
		if file != "" {
			next, err := l.load(filepath.Join(filepath.Dir(d.path), file))
			if err != nil {
				return nil, nil, err
			}
			target = next
		}
		var cur any = target.root
		for _, part := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
			m, ok := cur.(map[string]any)
			if !ok {
				return nil, nil, fmt.Errorf("bad ref %s in %s", ref, d.path)
			}
			if cur, ok = m[part]; !ok {
				return nil, nil, fmt.Errorf("missing ref %s in %s", ref, d.path)
			}
		}
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("ref %s in %s is not an object", ref, d.path)
		}
		d, node = target, m
	}
	return nil, nil, fmt.Errorf("ref chain too deep in %s", d.path)
}

type operation struct {
	method, path, id string
	node             map[string]any
}

func operations(d *doc) []operation {
	var ops []operation
	paths, _ := d.root["paths"].(map[string]any)
	for path, item := range paths {
		methods, _ := item.(map[string]any)
		for method, op := range methods {
			switch method {
			case "get", "post", "put", "patch", "delete":
				node, _ := op.(map[string]any)
				id, _ := node["operationId"].(string)
				ops = append(ops, operation{method, path, id, node})
			}
		}
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].path+ops[i].method < ops[j].path+ops[j].method })
	return ops
}

func contractFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(contractsDir, "*", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no contracts found, so this test would prove nothing")
	}
	return files
}

func eachOperation(t *testing.T, check func(t *testing.T, l *loader, d *doc, op operation)) {
	t.Helper()
	for _, file := range contractFiles(t) {
		l := &loader{docs: map[string]*doc{}}
		d, err := l.load(file)
		if err != nil {
			t.Fatal(err)
		}
		ops := operations(d)
		if len(ops) == 0 {
			t.Fatalf("%s has no operations", file)
		}
		for _, op := range ops {
			t.Run(op.id, func(t *testing.T) { check(t, l, d, op) })
		}
	}
}

func hasIdempotencyKey(l *loader, d *doc, op operation) (bool, error) {
	params, _ := op.node["parameters"].([]any)
	for _, p := range params {
		node, _ := p.(map[string]any)
		_, target, err := l.resolve(d, node)
		if err != nil {
			return false, err
		}
		if target["name"] == "Idempotency-Key" && target["required"] == true {
			return true, nil
		}
	}
	return false, nil
}

func TestWritesRequireIdempotencyKey(t *testing.T) {
	eachOperation(t, func(t *testing.T, l *loader, d *doc, op operation) {
		if op.method == "get" {
			return
		}
		has, err := hasIdempotencyKey(l, d, op)
		if err != nil {
			t.Fatal(err)
		}
		if exempt := operationsWithoutIdempotency[op.id]; has == exempt {
			t.Errorf("%s %s: has Idempotency-Key=%v, exempt=%v", op.method, op.path, has, exempt)
		}
	})
}

func TestSuccessResponsesCarryServerTime(t *testing.T) {
	eachOperation(t, func(t *testing.T, l *loader, d *doc, op operation) {
		responses, _ := op.node["responses"].(map[string]any)
		found := false
		for status, r := range responses {
			if !strings.HasPrefix(status, "2") {
				continue
			}
			found = true
			schema := jsonSchema(t, l, d, r)
			if schema.node == nil {
				t.Errorf("%s: %s has no JSON body", op.id, status)
				continue
			}
			requireProperty(t, op.id+" "+status, schema.node, "server_time")
			requireServerTimeType(t, op.id+" "+status, l, schema, "server_time")
		}
		if !found {
			t.Errorf("%s has no success response", op.id)
		}
	})
}

func TestErrorResponsesUseTheErrorShape(t *testing.T) {
	eachOperation(t, func(t *testing.T, l *loader, d *doc, op operation) {
		responses, _ := op.node["responses"].(map[string]any)
		for status, r := range responses {
			if strings.HasPrefix(status, "2") {
				continue
			}
			schema := jsonSchema(t, l, d, r)
			if schema.node == nil {
				t.Errorf("%s: %s has no JSON body", op.id, status)
				continue
			}
			for _, field := range []string{"code", "message", "retryable", "server_time"} {
				requireProperty(t, op.id+" "+status, schema.node, field)
			}
			requireServerTimeType(t, op.id+" "+status, l, schema, "server_time")
		}
		if _, ok := responses["500"]; !ok {
			t.Errorf("%s does not declare a 500", op.id)
		}
	})
}

func TestSignInIsDeclaredCorrectly(t *testing.T) {
	eachOperation(t, func(t *testing.T, l *loader, d *doc, op operation) {
		security, overridden := op.node["security"].([]any)
		open := overridden && len(security) == 0
		if open != operationsWithoutBearer[op.id] {
			t.Errorf("%s %s: open=%v, allowed open=%v", op.method, op.path, open, operationsWithoutBearer[op.id])
		}
		responses, _ := op.node["responses"].(map[string]any)
		if _, has401 := responses["401"]; !open && !has401 {
			t.Errorf("%s needs sign-in but does not declare a 401", op.id)
		}
	})
}

func TestListsUseCursorPaging(t *testing.T) {
	eachOperation(t, func(t *testing.T, l *loader, d *doc, op operation) {
		names := map[string]bool{}
		params, _ := op.node["parameters"].([]any)
		for _, p := range params {
			node, _ := p.(map[string]any)
			_, target, err := l.resolve(d, node)
			if err != nil {
				t.Fatal(err)
			}
			name, _ := target["name"].(string)
			names[name] = true
			if name == "offset" || name == "page" {
				t.Errorf("%s uses %v paging", op.id, name)
			}
		}
		responses, _ := op.node["responses"].(map[string]any)
		for status, r := range responses {
			if !strings.HasPrefix(status, "2") {
				continue
			}
			schema := jsonSchema(t, l, d, r)
			props, _ := schema.node["properties"].(map[string]any)
			if _, paged := props["next_cursor"]; paged && (!names["cursor"] || !names["limit"]) {
				t.Errorf("%s returns next_cursor without cursor and limit parameters", op.id)
			}
		}
	})
}

func TestEveryContractRequiresSignInByDefault(t *testing.T) {
	for _, file := range contractFiles(t) {
		l := &loader{docs: map[string]*doc{}}
		d, err := l.load(file)
		if err != nil {
			t.Fatal(err)
		}
		security, _ := d.root["security"].([]any)
		if len(security) == 0 {
			t.Errorf("%s has no default security, so every endpoint would be open", file)
		}
	}
}

func TestRejectionsAreDeclared(t *testing.T) {
	eachOperation(t, func(t *testing.T, l *loader, d *doc, op operation) {
		responses, _ := op.node["responses"].(map[string]any)
		has, err := hasIdempotencyKey(l, d, op)
		if err != nil {
			t.Fatal(err)
		}
		if has {
			for _, status := range []string{"400", "409", "422"} {
				if _, ok := responses[status]; !ok {
					t.Errorf("%s has Idempotency-Key but does not declare a %s", op.id, status)
				}
			}
		}
		params, _ := op.node["parameters"].([]any)
		for _, p := range params {
			node, _ := p.(map[string]any)
			_, target, err := l.resolve(d, node)
			if err != nil {
				t.Fatal(err)
			}
			if target["in"] == "query" && target["required"] == true {
				if _, ok := responses["400"]; !ok {
					t.Errorf("%s requires %v but does not declare a 400", op.id, target["name"])
				}
			}
		}
	})
}

// customerOrderFields is the complete list of what a customer may see of an order.
// A new field must be added here on purpose, after checking it cannot reveal the rider.
var customerOrderFields = []string{
	"cancel_until", "cancellable", "created_at", "customer_stage", "ended_reason", "eta_offer", "eta_s",
	"eta_source", "gate", "handover_otp", "id", "items", "order_number", "payment_request", "payment_status",
	"remedy", "rider", "seq", "timer", "total", "wait_deadline",
}

func TestCustomerOrderFieldsAreExactlyTheAllowedList(t *testing.T) {
	l := &loader{docs: map[string]*doc{}}
	d, err := l.load(filepath.Join(contractsDir, "customer/openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	components, _ := d.root["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	order, _ := schemas["Order"].(map[string]any)
	props, _ := order["properties"].(map[string]any)
	required, _ := order["required"].([]any)

	var got []string
	for name := range props {
		got = append(got, name)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(customerOrderFields, ",") {
		t.Errorf("Order fields changed.\n got: %v\nwant: %v", got, customerOrderFields)
	}
	requiredSet := map[string]bool{}
	for _, r := range required {
		name, _ := r.(string)
		requiredSet[name] = true
	}
	for _, name := range customerOrderFields {
		if !requiredSet[name] {
			t.Errorf("Order.%s must be required, so a stage can never leave it out", name)
		}
	}
}

func TestOperationIdsAreUniquePerContract(t *testing.T) {
	for _, file := range contractFiles(t) {
		l := &loader{docs: map[string]*doc{}}
		d, err := l.load(file)
		if err != nil {
			t.Fatal(err)
		}
		ids := map[string]bool{}
		for _, op := range operations(d) {
			if ids[op.id] {
				t.Errorf("%s: duplicate operationId %s", file, op.id)
			}
			ids[op.id] = true
		}
	}
}

// TestCustomerContractHidesTheRider walks everything reachable from the customer
// contract, so a hidden field cannot arrive through a shared schema either.
func TestCustomerContractHidesTheRider(t *testing.T) {
	for _, file := range []string{"customer/openapi.yaml", "customer/stream.asyncapi.yaml"} {
		l := &loader{docs: map[string]*doc{}}
		d, err := l.load(filepath.Join(contractsDir, file))
		if err != nil {
			t.Fatal(err)
		}
		start := d.root
		if paths, ok := d.root["paths"]; ok {
			start = map[string]any{"paths": paths}
		}
		seen := map[string]bool{}
		if err := walkForHidden(l, d, start, file, seen, hiddenCustomerFields, hiddenCustomerRefs); err != nil {
			t.Error(err)
		}
		if len(seen) == 0 {
			t.Errorf("%s: followed no references, so this test proved nothing", file)
		}
	}
}

// TestRiderContractHasNoCountdown protects the owner's rule: the rider sees a calm
// fixed label, so the clock never pressures the rider.
func TestRiderContractHasNoCountdown(t *testing.T) {
	for _, file := range []string{"rider/openapi.yaml", "rider/stream.asyncapi.yaml"} {
		l := &loader{docs: map[string]*doc{}}
		d, err := l.load(filepath.Join(contractsDir, file))
		if err != nil {
			t.Fatal(err)
		}
		start := d.root
		if paths, ok := d.root["paths"]; ok {
			start = map[string]any{"paths": paths}
		}
		seen := map[string]bool{}
		if err := walkForHidden(l, d, start, file, seen, hiddenRiderFields, hiddenRiderRefs); err != nil {
			t.Error(err)
		}
		if len(seen) == 0 {
			t.Errorf("%s: followed no references, so this test proved nothing", file)
		}
	}
}

// riderOrderFields is the complete list of what a rider may see of an order.
// A new field must be added here on purpose, after checking it adds no countdown
// and no customer contact details.
var riderOrderFields = []string{
	"ended_reason", "fencing_token", "gate", "handed_over_at", "id", "items", "offer_id", "order_number",
	"otp_available", "rider_accepted_at", "seq", "state", "timer_label",
}

func TestRiderOrderFieldsAreExactlyTheAllowedList(t *testing.T) {
	l := &loader{docs: map[string]*doc{}}
	d, err := l.load(filepath.Join(contractsDir, "rider/openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	components, _ := d.root["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	order, _ := schemas["RiderOrder"].(map[string]any)
	props, _ := order["properties"].(map[string]any)
	required, _ := order["required"].([]any)
	var got []string
	for name := range props {
		got = append(got, name)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(riderOrderFields, ",") {
		t.Errorf("RiderOrder fields changed.\n got: %v\nwant: %v", got, riderOrderFields)
	}
	requiredSet := map[string]bool{}
	for _, r := range required {
		name, _ := r.(string)
		requiredSet[name] = true
	}
	for _, name := range riderOrderFields {
		if !requiredSet[name] {
			t.Errorf("RiderOrder.%s must be required", name)
		}
	}
}

// TestRiderCannotReportArrival protects decision 0007: GPS stops the timer, so
// there must be no arrive endpoint.
func TestRiderCannotReportArrival(t *testing.T) {
	l := &loader{docs: map[string]*doc{}}
	d, err := l.load(filepath.Join(contractsDir, "rider/openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range operations(d) {
		if strings.Contains(strings.ToLower(op.path), "arriv") {
			t.Errorf("%s %s: arrival comes from GPS, not an endpoint", op.method, op.path)
		}
	}
}

func walkForHidden(l *loader, d *doc, node any, at string, seen map[string]bool, fields map[string]bool, refs *regexp.Regexp) error {
	switch v := node.(type) {
	case []any:
		for i, item := range v {
			if err := walkForHidden(l, d, item, fmt.Sprintf("%s[%d]", at, i), seen, fields, refs); err != nil {
				return err
			}
		}
	case map[string]any:
		if ref, ok := v["$ref"].(string); ok {
			if refs.MatchString(ref) {
				return fmt.Errorf("%s references hidden schema %s", at, ref)
			}
			key := d.path + "|" + ref
			if seen[key] {
				return nil
			}
			seen[key] = true
			target, node, err := l.resolve(d, v)
			if err != nil {
				return err
			}
			return walkForHidden(l, target, node, at+"->"+ref, seen, fields, refs)
		}
		if props, ok := v["properties"].(map[string]any); ok {
			for name := range props {
				if fields[name] {
					return fmt.Errorf("%s exposes hidden field %q", at, name)
				}
			}
		}
		for key, child := range v {
			if err := walkForHidden(l, d, child, at+"."+key, seen, fields, refs); err != nil {
				return err
			}
		}
	}
	return nil
}

type located struct {
	doc  *doc
	node map[string]any
}

func jsonSchema(t *testing.T, l *loader, d *doc, response any) located {
	t.Helper()
	node, _ := response.(map[string]any)
	rd, target, err := l.resolve(d, node)
	if err != nil {
		t.Fatal(err)
	}
	content, _ := target["content"].(map[string]any)
	media, _ := content["application/json"].(map[string]any)
	schema, _ := media["schema"].(map[string]any)
	if schema == nil {
		return located{}
	}
	sd, resolved, err := l.resolve(rd, schema)
	if err != nil {
		t.Fatal(err)
	}
	return located{sd, resolved}
}

// requireServerTimeType checks the property really is the shared ServerTime, not just named that.
func requireServerTimeType(t *testing.T, where string, l *loader, schema located, name string) {
	t.Helper()
	props, _ := schema.node["properties"].(map[string]any)
	prop, _ := props[name].(map[string]any)
	if prop == nil {
		return
	}
	ref, _ := prop["$ref"].(string)
	if !strings.HasSuffix(ref, "#/components/schemas/ServerTime") {
		t.Errorf("%s: %q must reference the shared ServerTime", where, name)
	}
}

func requireProperty(t *testing.T, where string, schema map[string]any, name string) {
	t.Helper()
	props, _ := schema["properties"].(map[string]any)
	if _, ok := props[name]; !ok {
		t.Errorf("%s: missing property %q", where, name)
	}
	required, _ := schema["required"].([]any)
	for _, r := range required {
		if r == name {
			return
		}
	}
	t.Errorf("%s: property %q is not required", where, name)
}
