package jobs

import (
	"encoding/json"
	"math/rand/v2"
	"strconv"
	"testing"
)

func TestCanonicalJSON(t *testing.T) {
	type in struct {
		B string         `json:"b"`
		A int            `json:"a"`
		M map[string]any `json:"m"`
	}
	for _, tc := range []struct {
		name string
		v    any
		want string
	}{
		{"sorted keys", map[string]any{"b": 1, "a": []any{true, nil, "x"}}, `{"a":[true,null,"x"],"b":1}`},
		{"struct fields sorted", in{B: "x", A: 2, M: map[string]any{"z": 1, "y": 2}}, `{"a":2,"b":"x","m":{"y":2,"z":1}}`},
		{"no HTML escaping", map[string]string{"u": "<a&b>"}, `{"u":"<a&b>"}`},
		{"unicode kept", "字幕 é", `"字幕 é"`},
		{"numbers", []any{1, 1.0, json.RawMessage("1e0"), 0.2, -3, json.RawMessage("2.50"), 1e21, uint64(18446744073709551615), int64(-9223372036854775808)},
			`[1,1,1,2e-01,-3,2.5e+00,1e+21,18446744073709551615,-9223372036854775808]`},
		{"raw message is normalized", json.RawMessage(`{ "b" : 1 , "a" : { "d":[ ], "c":"x" } }`), `{"a":{"c":"x","d":[]},"b":1}`},
	} {
		got, err := CanonicalJSON(tc.v)
		if err != nil || string(got) != tc.want {
			t.Errorf("%s: got %s (%v), want %s", tc.name, got, err, tc.want)
		}
	}
	if _, err := CanonicalJSON(make(chan int)); err == nil {
		t.Error("unmarshalable value accepted")
	}
}

func TestFingerprint(t *testing.T) {
	base := Fingerprint("transcribe", 1, map[string]any{"audio": "abc", "lang": "zh", "vad": true})
	if len(base) != 64 {
		t.Fatalf("fingerprint %q is not hex sha256", base)
	}
	// Known value: guards the canonical form against accidental changes,
	// which would silently invalidate every cached artifact.
	const want = "0b59d98599f41de58e899f12518a7b70ce6d66689cea18c99e200759868461f1"
	if base != want {
		t.Fatalf("fingerprint of the reference input changed: %s", base)
	}

	// Map key order must not matter: Go randomizes iteration, so build many
	// maps with different insertion orders.
	keys := make([]string, 40)
	for i := range keys {
		keys[i] = "k" + strconv.Itoa(i)
	}
	ref := ""
	for range 50 {
		rand.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
		m := map[string]any{}
		for _, k := range keys {
			m[k] = map[string]any{"v": k, "n": len(k)}
		}
		fp := Fingerprint("s", 1, m)
		if ref == "" {
			ref = fp
		} else if fp != ref {
			t.Fatal("fingerprint depends on map order")
		}
	}

	// A struct and the equivalent map agree; so do equal numbers.
	type inputs struct {
		Lang  string  `json:"lang"`
		Audio string  `json:"audio"`
		VAD   bool    `json:"vad"`
		Temp  float64 `json:"temp"`
	}
	a := Fingerprint("x", 2, inputs{Lang: "zh", Audio: "abc", VAD: true, Temp: 1})
	b := Fingerprint("x", 2, map[string]any{"vad": true, "audio": "abc", "lang": "zh", "temp": json.RawMessage("1.0")})
	if a != b {
		t.Fatal("struct and map with the same content differ")
	}

	// Every component is significant.
	distinct := map[string]string{
		"base":          base,
		"stage":         Fingerprint("transcribe2", 1, map[string]any{"audio": "abc", "lang": "zh", "vad": true}),
		"stage_version": Fingerprint("transcribe", 2, map[string]any{"audio": "abc", "lang": "zh", "vad": true}),
		"value":         Fingerprint("transcribe", 1, map[string]any{"audio": "abd", "lang": "zh", "vad": true}),
		"key":           Fingerprint("transcribe", 1, map[string]any{"audio2": "abc", "lang": "zh", "vad": true}),
		"extra":         Fingerprint("transcribe", 1, map[string]any{"audio": "abc", "lang": "zh", "vad": true, "x": nil}),
		"nil inputs":    Fingerprint("transcribe", 1, nil),
	}
	seen := map[string]string{}
	for name, fp := range distinct {
		if other, dup := seen[fp]; dup {
			t.Errorf("%s and %s collide", name, other)
		}
		seen[fp] = name
	}
}
