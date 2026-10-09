package app

import (
	"crypto/rand"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

// ID prefixes (docs/v0.md §6.3, contract).
const (
	PrefixVideo      = "vid_"
	PrefixJob        = "job_"
	PrefixTranscript = "tr_"
	PrefixSegment    = "seg_"
	PrefixNote       = "note_"
	PrefixArtifact   = "art_"
	PrefixUpload     = "upl_"
	PrefixExport     = "exp_"

	// SegmentLinkScheme is the canonical citation link prefix: wk://seg/<segment_id>.
	SegmentLinkScheme = "wk://seg/"
)

var (
	ulidMu      sync.Mutex
	ulidEntropy = ulid.Monotonic(rand.Reader, 0)
)

// NewID returns prefix + a monotonic ULID.
func NewID(prefix string) string {
	ulidMu.Lock()
	defer ulidMu.Unlock()
	return prefix + ulid.MustNew(ulid.Timestamp(time.Now()), ulidEntropy).String()
}

// SegmentID builds seg_<transcript ULID>_<5-digit idx>; idx is 1-based.
func SegmentID(transcriptID string, idx int) string {
	return fmt.Sprintf("%s%s_%05d", PrefixSegment, strings.TrimPrefix(transcriptID, PrefixTranscript), idx)
}

// ParseSegmentID splits a segment ID into its transcript ID and 1-based idx.
func ParseSegmentID(id string) (transcriptID string, idx int, ok bool) {
	rest, found := strings.CutPrefix(id, PrefixSegment)
	if !found {
		return "", 0, false
	}
	u, n, found := strings.Cut(rest, "_")
	if !found || len(u) != ulid.EncodedSize || len(n) != 5 {
		return "", 0, false
	}
	for _, c := range n {
		if c < '0' || c > '9' {
			return "", 0, false
		}
		idx = idx*10 + int(c-'0')
	}
	if idx < 1 {
		return "", 0, false
	}
	return PrefixTranscript + u, idx, true
}

// Now returns the current instant as an RFC 3339 UTC string.
func Now() string { return FormatTime(time.Now()) }

// FormatTime renders t as the RFC 3339 UTC string used on the wire and in SQLite.
func FormatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
