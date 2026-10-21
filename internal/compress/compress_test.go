package compress

import (
	"strings"
	"testing"
)

func TestCompressesWhitespaceAndPunctuation(t *testing.T) {
	in := "Hello    world ,   this is   fine !!!!!\n\n\n\nNext   line ."
	out, st := Text(in)
	if out != "Hello world, this is fine!\n\nNext line." {
		t.Fatalf("out=%q", out)
	}
	if st.OriginalChars <= st.CompressedChars || st.TokensSaved <= 0 {
		t.Fatalf("stats=%+v", st)
	}
}

func TestPreservesCodeFences(t *testing.T) {
	code := "```go\nfunc  main()  {\n    x  :=  1  // keep\n}\n```"
	in := "Explain   this :\n" + code + "\n\n\nthanks !!"
	out, _ := Text(in)
	if !strings.Contains(out, "func  main()  {\n    x  :=  1  // keep\n}") {
		t.Fatalf("code block was modified: %q", out)
	}
	if !strings.HasPrefix(out, "Explain this:\n```go") {
		t.Fatalf("prose around code not compressed: %q", out)
	}
}

func TestNoOpOnTightText(t *testing.T) {
	in := "already tight."
	out, st := Text(in)
	if out != in || st.TokensSaved != 0 {
		t.Fatalf("out=%q stats=%+v", out, st)
	}
}

func TestNeverDropsWords(t *testing.T) {
	in := "one two three four five"
	out, _ := Text(in)
	if out != in {
		t.Fatalf("words changed: %q", out)
	}
}

func TestEmptyAndWhitespaceOnly(t *testing.T) {
	if out, _ := Text(""); out != "" {
		t.Fatalf("empty -> %q", out)
	}
	out, _ := Text("   \n\t ")
	if strings.TrimSpace(out) != "" {
		t.Fatalf("whitespace only -> %q", out)
	}
}
