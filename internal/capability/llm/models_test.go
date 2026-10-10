package llm_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"watchink/internal/app"
	"watchink/internal/capability/llm"
)

func TestListModels(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   []string
		code   app.Code
	}{
		{"openai shape, sorted and deduplicated", 200, `{"object":"list","data":[{"id":"m-b"},{"id":"m-a"},{"id":"m-b"}]}`, []string{"m-a", "m-b"}, ""},
		{"models + name shape", 200, `{"models":[{"name":"models/gem-1"}]}`, []string{"gem-1"}, ""},
		{"bare array", 200, `[{"id":"x"}]`, []string{"x"}, ""},
		{"unauthorized", 401, `{"error":{"message":"bad key"}}`, nil, app.CodeModelAuthFailed},
		{"no listing", 404, `not found`, nil, app.CodeModelUnavailable},
		{"empty", 200, `{"data":[]}`, nil, app.CodeModelUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotAuth, gotPath string
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer ts.Close()
			got, err := llm.ListModels(context.Background(), llm.Config{BaseURL: ts.URL + "/v1/", APIKey: testKey})
			if tc.code != "" {
				if !app.IsCode(err, tc.code) {
					t.Fatalf("err = %v, want %s", err, tc.code)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v %v, want %v", got, err, tc.want)
			}
			if gotPath != "/v1/models" || gotAuth != "Bearer "+testKey {
				t.Fatalf("request path = %s, authorized = %v", gotPath, gotAuth != "")
			}
		})
	}
	if _, err := llm.ListModels(context.Background(), llm.Config{}); !app.IsCode(err, app.CodeModelNotConfigured) {
		t.Fatalf("missing base_url: %v", err)
	}
}
