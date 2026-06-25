package flysprite

import (
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestSessionAndSpriteName(t *testing.T) {
	if got, err := sessionName("proj-1"); err != nil || got != "proj-1" {
		t.Fatalf("sessionName(proj-1) = %q, %v", got, err)
	}
	if got, err := spriteName("Proj-1"); err != nil || got != "ao-proj-1" {
		t.Fatalf("spriteName(Proj-1) = %q, %v; want ao-proj-1", got, err)
	}
	for _, bad := range []domain.SessionID{"", "has space", "semi;colon", "slash/x"} {
		if _, err := sessionName(bad); err == nil {
			t.Errorf("sessionName(%q): want error, got nil", bad)
		}
		if _, err := spriteName(bad); err == nil {
			t.Errorf("spriteName(%q): want error, got nil", bad)
		}
	}
}

func TestFlyHandleRoundTrip(t *testing.T) {
	h := flyHandle{Sprite: "ao-proj-1", Session: "proj-1", Pane: "terminal_1"}
	got, err := parseFlyHandle(h.encode())
	if err != nil {
		t.Fatalf("parseFlyHandle: %v", err)
	}
	if got != h {
		t.Fatalf("round trip = %+v, want %+v", got, h)
	}
	for _, bad := range []string{"", "only-one", "a\x1f", "a\x1fb"} {
		if _, err := parseFlyHandle(bad); err == nil {
			t.Errorf("parseFlyHandle(%q): want error, got nil", bad)
		}
	}
}

func TestNormalizeAgentArgv0(t *testing.T) {
	got := normalizeAgentArgv0([]string{"/opt/homebrew/bin/claude", "--session-id", "x", "--", "do it"})
	if got[0] != "claude" {
		t.Fatalf("argv[0] = %q, want claude", got[0])
	}
	// A bare command is left untouched.
	if got := normalizeAgentArgv0([]string{"claude", "--foo"}); got[0] != "claude" {
		t.Fatalf("bare argv[0] = %q, want claude", got[0])
	}
	// Input is not mutated.
	in := []string{"/usr/bin/claude"}
	_ = normalizeAgentArgv0(in)
	if in[0] != "/usr/bin/claude" {
		t.Fatalf("input mutated: %q", in[0])
	}
}

func TestBuildLayout(t *testing.T) {
	layout := buildLayout("/workspace/repo", []string{"claude", "--", "fix the bug"}, map[string]string{
		"AO_SESSION_ID": "proj-1",
		"PATH":          "/should/not/appear",
		"B_VAR":         "two",
		"A_VAR":         "one",
	})
	for _, want := range []string{
		`cwd "/workspace/repo"`,
		`command="bash"`,
		`name="agent"`,
		`args "-lc"`,
		`export AO_SESSION_ID='proj-1';`,
		`'fix the bug'`, // prompt stays one shell word
	} {
		if !strings.Contains(layout, want) {
			t.Errorf("layout missing %q\n---\n%s", want, layout)
		}
	}
	if strings.Contains(layout, "/should/not/appear") {
		t.Errorf("PATH must not be exported into the layout:\n%s", layout)
	}
	// env exported in sorted key order: A_VAR before B_VAR.
	if strings.Index(layout, "A_VAR") > strings.Index(layout, "B_VAR") {
		t.Errorf("env not exported in sorted order:\n%s", layout)
	}
}

func TestCommandArgShapes(t *testing.T) {
	create := createSessionArgs("proj-1", "/tmp/l.kdl")
	if create[0] != "attach" || create[1] != "--create-background" || create[2] != "proj-1" {
		t.Fatalf("createSessionArgs prefix wrong: %v", create)
	}
	if !containsSeq(create, "--default-layout", "/tmp/l.kdl") {
		t.Fatalf("createSessionArgs missing layout: %v", create)
	}
	if del := deleteSessionArgs("proj-1"); del[0] != "delete-session" || del[1] != "--force" || del[2] != "proj-1" {
		t.Fatalf("deleteSessionArgs wrong: %v", del)
	}
	if at := attachArgs("proj-1"); at[0] != "attach" || at[1] != "proj-1" {
		t.Fatalf("attachArgs wrong: %v", at)
	}
}

func containsSeq(args []string, a, b string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == a && args[i+1] == b {
			return true
		}
	}
	return false
}
