package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestQueueLogfStartsWithRFC3339UTCTime(t *testing.T) {
	var out bytes.Buffer
	at := time.Date(2026, 11, 23, 15, 55, 7, 0, time.FixedZone("CET", 3600))
	r := &queueRunner{env: queueEnv{out: &out, now: func() time.Time { return at }}}
	r.logf("queue: %s", "stopping")
	stamp, rest, ok := strings.Cut(strings.TrimSuffix(out.String(), "\n"), " ")
	if !ok || rest != "queue: stopping" {
		t.Fatalf("line = %q", out.String())
	}
	got, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		t.Fatalf("stamp %q does not parse: %v", stamp, err)
	}
	if stamp != "2026-11-23T14:55:07Z" || !got.Equal(at) {
		t.Fatalf("stamp = %q, want 2026-11-23T14:55:07Z", stamp)
	}
}
