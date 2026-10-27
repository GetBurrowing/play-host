package dryrun

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// MinProbes is the smallest n a probe row may have.
const MinProbes = 100

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// FailError is the 'FAIL: <field> missing|invalid' line dryrun-check prints.
type FailError struct{ Field, Why string }

func (e *FailError) Error() string { return fmt.Sprintf("FAIL: %s %s", e.Field, e.Why) }

func missing(f string) error { return &FailError{f, "missing"} }
func invalid(f string) error { return &FailError{f, "invalid"} }

// Check returns nil only when every field is present and numeric where it
// must be, every probe row has n >= MinProbes, and the canvas session
// reached live. It reads the raw JSON so an absent key is told apart from a
// zero value.
func Check(b []byte) error {
	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		return invalid("report")
	}
	for _, f := range []string{"sha", "ue_version", "gpu", "capture_backend", "encoder"} {
		v, ok := root[f]
		if !ok {
			return missing(f)
		}
		s, isStr := v.(string)
		if !isStr || s == "" || (f == "sha" && !shaRe.MatchString(s)) {
			return invalid(f)
		}
	}
	bm, err := object(root, "build_ms", "build_ms")
	if err != nil {
		return err
	}
	for _, ph := range []string{"cold", "warm"} {
		path := "build_ms." + ph
		p, err := object(bm, ph, path)
		if err != nil {
			return err
		}
		if _, err := number(p, "total", path+".total"); err != nil {
			return err
		}
		steps, err := object(p, "per_step", path+".per_step")
		if err != nil {
			return err
		}
		for k := range steps {
			if _, err := number(steps, k, path+".per_step."+k); err != nil {
				return err
			}
		}
	}
	rows, ok := root["probe"]
	if !ok {
		return missing("probe")
	}
	list, ok := rows.([]any)
	if !ok || len(list) == 0 {
		return invalid("probe")
	}
	for i, r := range list {
		path := fmt.Sprintf("probe[%d]", i)
		row, ok := r.(map[string]any)
		if !ok {
			return invalid(path)
		}
		for _, f := range []string{"backend", "preset"} {
			v, ok := row[f]
			if !ok {
				return missing(path + "." + f)
			}
			if s, isStr := v.(string); !isStr || s == "" {
				return invalid(path + "." + f)
			}
		}
		k, ok := row["kind"]
		if !ok {
			return missing(path + ".kind")
		}
		if k != KindHostToPhoton {
			return invalid(path + ".kind")
		}
		n, err := number(row, "n", path+".n")
		if err != nil {
			return err
		}
		if n < MinProbes {
			return invalid(path + ".n")
		}
		if _, err := number(row, "lost", path+".lost"); err != nil {
			return err
		}
		p50, err := number(row, "p50_ms", path+".p50_ms")
		if err != nil {
			return err
		}
		p95, err := number(row, "p95_ms", path+".p95_ms")
		if err != nil {
			return err
		}
		if p95 < p50 {
			return invalid(path + ".p95_ms")
		}
		if _, err := number(row, "fps", path+".fps"); err != nil {
			return err
		}
	}
	cs, err := object(root, "canvas_session", "canvas_session")
	if err != nil {
		return err
	}
	id, ok := cs["session_id"]
	if !ok {
		return missing("canvas_session.session_id")
	}
	if s, isStr := id.(string); !isStr || s == "" {
		return invalid("canvas_session.session_id")
	}
	live, ok := cs["reached_live"]
	if !ok {
		return missing("canvas_session.reached_live")
	}
	if l, isBool := live.(bool); !isBool || !l {
		return invalid("canvas_session.reached_live")
	}
	for _, f := range []string{"rtt_p50_ms", "rtt_p95_ms"} {
		if _, err := number(cs, f, "canvas_session."+f); err != nil {
			return err
		}
	}
	return nil
}

func object(m map[string]any, key, path string) (map[string]any, error) {
	v, ok := m[key]
	if !ok {
		return nil, missing(path)
	}
	o, ok := v.(map[string]any)
	if !ok {
		return nil, invalid(path)
	}
	return o, nil
}

func number(m map[string]any, key, path string) (float64, error) {
	v, ok := m[key]
	if !ok {
		return 0, missing(path)
	}
	f, ok := v.(float64)
	if !ok || f < 0 {
		return 0, invalid(path)
	}
	return f, nil
}
