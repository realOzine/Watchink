package jobs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
)

// Fingerprint returns the reuse key of a stage run: the lowercase hex
// SHA-256 of the canonical JSON of
//
//	{"inputs": <inputs>, "stage": <stage>, "stage_version": <stageVersion>}
//
// inputs is any JSON-marshalable value holding everything the stage output
// depends on (input hashes, parameters, tool versions, model IDs). Bump
// stageVersion when the stage logic changes.
//
// Canonical means: object keys sorted, no insignificant whitespace, no HTML
// escaping, and numbers normalized (1, 1.0 and 1e0 are the same value), so
// equal inputs give equal fingerprints regardless of map iteration order or
// struct field order.
//
// It panics when inputs cannot be marshalled to JSON, which is a programming
// error in the stage.
func Fingerprint(stage string, stageVersion int, inputs any) string {
	b, err := CanonicalJSON(map[string]any{"stage": stage, "stage_version": stageVersion, "inputs": inputs})
	if err != nil {
		panic(fmt.Sprintf("jobs.Fingerprint(%s): %v", stage, err))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// CanonicalJSON marshals v to the canonical form described at Fingerprint.
func CanonicalJSON(v any) ([]byte, error) {
	var raw bytes.Buffer
	enc := json.NewEncoder(&raw)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// Round-trip through the generic representation so structs, maps and
	// json.RawMessage all end up as the same tree; UseNumber keeps integers
	// beyond 2^53 exact.
	dec := json.NewDecoder(&raw)
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := writeCanonical(&out, tree); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func writeCanonical(w *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		w.WriteString("null")
	case bool:
		w.WriteString(strconv.FormatBool(x))
	case string:
		return writeString(w, x)
	case json.Number:
		s, err := canonicalNumber(x)
		if err != nil {
			return err
		}
		w.WriteString(s)
	case []any:
		w.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				w.WriteByte(',')
			}
			if err := writeCanonical(w, e); err != nil {
				return err
			}
		}
		w.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		w.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				w.WriteByte(',')
			}
			if err := writeString(w, k); err != nil {
				return err
			}
			w.WriteByte(':')
			if err := writeCanonical(w, x[k]); err != nil {
				return err
			}
		}
		w.WriteByte('}')
	default:
		return fmt.Errorf("canonical JSON: unexpected type %T", v)
	}
	return nil
}

func writeString(w *bytes.Buffer, s string) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return err
	}
	w.Truncate(w.Len() - 1) // Encode appends a newline
	return nil
}

// canonicalNumber gives every numeric value one spelling: integers in the
// int64 / uint64 range are written in plain decimal, everything else in the
// shortest exponent form that round-trips as a float64.
func canonicalNumber(n json.Number) (string, error) {
	s := n.String()
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return strconv.FormatInt(i, 10), nil
	}
	if u, err := strconv.ParseUint(s, 10, 64); err == nil {
		return strconv.FormatUint(u, 10), nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return "", fmt.Errorf("canonical JSON: invalid number %q", s)
	}
	if f == math.Trunc(f) && math.Abs(f) < 1<<53 {
		return strconv.FormatInt(int64(f), 10), nil
	}
	return strconv.FormatFloat(f, 'e', -1, 64), nil
}
