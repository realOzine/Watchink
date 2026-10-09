package procx

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		secrets []string
		want    string
	}{
		{"plain text untouched", "frame=  100 fps= 25 time=00:00:04.00", nil, "frame=  100 fps= 25 time=00:00:04.00"},
		{"empty", "", []string{"abcd"}, ""},
		{"explicit secret", "token is hunter2-xyz here", []string{"hunter2-xyz"}, "token is *** here"},
		{"short secret ignored", "a b c", []string{"a"}, "a b c"},
		{"overlapping secrets", "x=abcdefgh", []string{"abcd", "abcdefgh"}, "x=***"},
		{"authorization bearer", "> Authorization: Bearer abc.def-123", nil, "> Authorization: Bearer ***"},
		{"authorization raw", "authorization: 0123456789abcdef", nil, "authorization: ***"},
		{"authorization json", `{"Authorization":"Bearer zzz999zzz"}`, nil, `{"Authorization":"Bearer ***"}`},
		{"bare bearer", "sent bearer AbCdEf123456 to host", nil, "sent bearer *** to host"},
		{"sk key", "key sk-proj-AbCdEf0123456789 rejected", nil, "key sk-*** rejected"},
		{"api_key assignment", "api_key=AKIA1234 other=1", nil, "api_key=*** other=1"},
		{"api-key header", "X-Api-Key: qwertyuiop", nil, "X-Api-Key: ***"},
		{"json api key", `"api_key": "s3cr3tvalue"`, nil, `"api_key": "***"`},
		{"env style", "OPENAI_API_KEY=abc123def", nil, "OPENAI_API_KEY=***"},
		{"media token", "GET /media/vid_1/playback?mt=eyJhbGciOi.abc&x=1", nil, "GET /media/vid_1/playback?mt=***&x=1"},
		{"login code", "http://127.0.0.1:1234/login?code=987654", nil, "http://127.0.0.1:1234/login?code=***"},
		{"second query param", "http://h/p?a=1&code=zzz#frag", nil, "http://h/p?a=1&code=***#frag"},
		{"exit code not a query", "exit code=1", nil, "exit code=1"},
		{"token count untouched", "n_tokens = 12, max_tokens=100", nil, "n_tokens = 12, max_tokens=100"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Redact(tt.in, tt.secrets...); got != tt.want {
				t.Errorf("Redact(%q)\n got %q\nwant %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestTailBuffer(t *testing.T) {
	tb := newTailBuffer(10)
	for _, s := range []string{"0123", "4567", "89ab", "cd"} {
		if n, err := tb.Write([]byte(s)); n != len(s) || err != nil {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
	if got := tb.String(); got != "456789abcd" {
		t.Errorf("tail = %q", got)
	}
	_, _ = tb.Write([]byte(strings.Repeat("x", 25) + "END"))
	if got := tb.String(); got != "xxxxxxxEND" {
		t.Errorf("tail after big write = %q", got)
	}

	// A multi-byte rune cut by the window must not produce invalid UTF-8.
	tb = newTailBuffer(4)
	_, _ = tb.Write([]byte("课程ab")) // 3+3+2 bytes: the window starts inside "程"
	if got := tb.String(); got != "ab" {
		t.Errorf("utf8 tail = %q", got)
	}
}

func TestLineWriter(t *testing.T) {
	var got []string
	w := &lineWriter{fn: func(s string) { got = append(got, s) }}
	for _, chunk := range []string{"one\ntw", "o\r\nthree\rfour", "\n\nlast"} {
		_, _ = w.Write([]byte(chunk))
	}
	w.flush()
	want := []string{"one", "two", "three", "four", "last"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("lines = %q, want %q", got, want)
	}
}
