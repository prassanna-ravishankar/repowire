package cli

import (
	"strings"
	"testing"
)

func TestWhyMarksExpiredText(t *testing.T) {
	var out strings.Builder
	printWhyStage(&out, map[string]any{"stage": "created", "ts": "t1", "detail": map[string]any{"from_peer": "a", "to_peer": "b", "content_expired": true}})
	printWhyStage(&out, map[string]any{"stage": "closed", "ts": "t2", "detail": map[string]any{"close_reason": "acked"}})
	printWhyStage(&out, map[string]any{"stage": "closed", "ts": "t3", "detail": map[string]any{"close_reason": "acked", "reply": "done"}})
	want := "  t1  @a → @b\n    (text expired under retention)\n  t2  acked\n  t3  acked\n    done\n"
	if out.String() != want {
		t.Fatalf("output:\n%s\nwant:\n%s", out.String(), want)
	}
}
