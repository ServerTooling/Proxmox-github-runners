package pve

import (
	"testing"
	"time"
)

func TestReuseTags(t *testing.T) {
	g := Guest{RawTags: "gh-runner;" + KeptTag(time.Unix(1700000000, 0)) + ";" + ArmedTag(time.Unix(5, 0))}
	if g.TagTime("kept") != time.Unix(1700000000, 0) || g.TagTime("armed") != time.Unix(5, 0) || !g.TagTime("created").IsZero() {
		t.Fatalf("tag times: %v %v", g.TagTime("kept"), g.TagTime("armed"))
	}
	if ReuseTag("Example-Org", "App") != ReuseTag("example-org", "app") || ReuseTag("example-org", "app") == ReuseTag("example-org", "ap") {
		t.Fatal("reuse tag must ignore case and tell repos apart")
	}
	if c := LXCTagsCmd(9000, []string{"a", "b"}); c.Name != "pct" || len(c.Args) != 4 || c.Args[3] != "a;b" {
		t.Fatalf("cmd %v", c)
	}
}
