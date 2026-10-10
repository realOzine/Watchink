package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// SchemaFor generates a JSON Schema from a Go struct value (or pointer), so
// the schema sent to the model and the type the answer is decoded into cannot
// drift apart (§9.2).
//
// Field names come from the json tag; `desc:"…"` becomes the description and
// `enum:"a,b"` an enum. Every field is required and additionalProperties is
// false, which is what strict json_schema mode demands. Only keywords that all
// common OpenAI-compatible servers accept are emitted.
func SchemaFor(name string, v any) *Schema {
	t := reflect.TypeOf(v)
	var b bytes.Buffer
	writeSchema(&b, t, "", "")
	return &Schema{Name: name, Schema: json.RawMessage(b.Bytes())}
}

func writeSchema(b *bytes.Buffer, t reflect.Type, desc, enum string) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	b.WriteByte('{')
	switch t.Kind() {
	case reflect.Struct:
		b.WriteString(`"type":"object","properties":{`)
		var required []string
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if !f.IsExported() || name == "-" {
				continue
			}
			if name == "" {
				name = f.Name
			}
			if len(required) > 0 {
				b.WriteByte(',')
			}
			required = append(required, name)
			writeJSON(b, name)
			b.WriteByte(':')
			writeSchema(b, f.Type, f.Tag.Get("desc"), f.Tag.Get("enum"))
		}
		b.WriteString(`},"required":`)
		writeJSON(b, required)
		b.WriteString(`,"additionalProperties":false`)
	case reflect.Slice, reflect.Array:
		b.WriteString(`"type":"array","items":`)
		writeSchema(b, t.Elem(), "", enum)
		enum = ""
	case reflect.String:
		b.WriteString(`"type":"string"`)
	case reflect.Bool:
		b.WriteString(`"type":"boolean"`)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		b.WriteString(`"type":"integer"`)
	case reflect.Float32, reflect.Float64:
		b.WriteString(`"type":"number"`)
	default:
		panic(fmt.Sprintf("llm.SchemaFor: unsupported kind %s", t.Kind()))
	}
	if enum != "" {
		b.WriteString(`,"enum":`)
		writeJSON(b, strings.Split(enum, ","))
	}
	if desc != "" {
		b.WriteString(`,"description":`)
		writeJSON(b, desc)
	}
	b.WriteByte('}')
}

func writeJSON(b *bytes.Buffer, v any) {
	enc := json.NewEncoder(b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	b.Truncate(b.Len() - 1) // Encode appends a newline
}
