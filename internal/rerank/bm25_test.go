package rerank

import "testing"

func TestBM25RanksRelevantFirst(t *testing.T) {
	docs := []string{
		"quantum physics for beginners",
		"apple pie recipe with cinnamon",
		"apple orchard harvest in autumn",
	}
	out := New().Rerank("apple pie", docs, 0)
	if len(out) != 3 {
		t.Fatalf("results=%d", len(out))
	}
	if out[0].Index != 1 {
		t.Fatalf("top result index=%d want 1 (%+v)", out[0].Index, out[0])
	}
	if out[0].Score <= out[1].Score {
		t.Fatalf("scores not descending: %+v", out)
	}
	if out[len(out)-1].Index != 0 {
		t.Fatalf("irrelevant doc should rank last: %+v", out)
	}
}

func TestBM25TopNLimit(t *testing.T) {
	docs := []string{"alpha", "alpha beta", "beta"}
	out := New().Rerank("alpha", docs, 2)
	if len(out) != 2 {
		t.Fatalf("topN ignored: %d", len(out))
	}
	if out[0].Index != 0 {
		t.Fatalf("expected exact match first, got %+v", out)
	}
}

func TestBM25EmptyInputs(t *testing.T) {
	r := New()
	if got := r.Rerank("q", nil, 0); len(got) != 0 {
		t.Fatalf("nil docs -> %+v", got)
	}
	out := r.Rerank("", []string{"a", "b"}, 0)
	if len(out) != 2 || out[0].Index != 0 || out[1].Index != 1 {
		t.Fatalf("empty query should preserve order: %+v", out)
	}
}

func TestBM25StableTieOrder(t *testing.T) {
	docs := []string{"same text", "same text"}
	out := New().Rerank("same", docs, 0)
	if out[0].Index != 0 || out[1].Index != 1 {
		t.Fatalf("ties must keep input order: %+v", out)
	}
}

func TestTokenize(t *testing.T) {
	got := Tokenize("Hello, WORLD-42 你好")
	if len(got) < 3 {
		t.Fatalf("tokens=%v", got)
	}
	if got[0] != "hello" || got[1] != "world" {
		t.Fatalf("tokens=%v", got)
	}
}
