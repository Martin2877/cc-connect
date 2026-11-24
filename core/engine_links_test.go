package core

import (
	"strings"
	"testing"
)

func newLinksTest() (*Engine, *stubPlatformEngine) {
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	return e, p
}

func TestCmdLinks_NotConfigured(t *testing.T) {
	e, p := newLinksTest()
	e.cmdLinks(p, &Message{SessionKey: "s1"}, nil)

	sent := p.getSent()
	if len(sent) != 1 {
		t.Fatalf("expected 1 reply, got %d", len(sent))
	}
	if !strings.Contains(sent[0], "projects.links") {
		t.Fatalf("expected not-configured hint, got: %s", sent[0])
	}
}

func TestCmdLinks_RendersAllConfigured(t *testing.T) {
	e, p := newLinksTest()
	e.SetLinks(map[string]string{
		"ide":     "https://code.example.com/?folder={work_dir}",
		"term":    "https://term.example.com/",
		"web":     "https://cc.example.com/",
		"preview": "https://p{port}.example.com/",
	})

	e.cmdLinks(p, &Message{SessionKey: "s1"}, nil)

	sent := p.getSent()
	if len(sent) == 0 {
		t.Fatal("expected a reply")
	}
	joined := strings.Join(sent, "\n")
	for _, want := range []string{
		"code.example.com",
		"term.example.com",
		"cc.example.com",
		"p{port}.example.com", // {port} stays literal until /links preview <port>
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected %q in reply, got: %s", want, joined)
		}
	}
	// {work_dir} must be expanded — no literal placeholder left in the ide url.
	if strings.Contains(joined, "folder={work_dir}") {
		t.Fatalf("work_dir placeholder not expanded: %s", joined)
	}
}

func TestCmdLinks_PreviewExpandsPort(t *testing.T) {
	e, p := newLinksTest()
	e.SetLinks(map[string]string{
		"preview": "https://p{port}.example.com/{project}",
	})

	e.cmdLinks(p, &Message{SessionKey: "s1"}, []string{"preview", "5173"})

	sent := p.getSent()
	if len(sent) != 1 || !strings.Contains(sent[0], "https://p5173.example.com/test") {
		t.Fatalf("expected expanded preview url, got: %v", sent)
	}
}

func TestCmdLinks_PreviewUsageError(t *testing.T) {
	e, p := newLinksTest()
	e.SetLinks(map[string]string{"preview": "https://p{port}.example.com/"})

	e.cmdLinks(p, &Message{SessionKey: "s1"}, []string{"preview"})

	sent := p.getSent()
	if len(sent) != 1 || !strings.Contains(sent[0], "preview") {
		t.Fatalf("expected usage hint, got: %v", sent)
	}
}
