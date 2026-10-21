package model

import (
	"encoding/json"
	"testing"
)

func TestParseNUMERICRoundTrip(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"0.0000000000", "0.00"},
		{"1", "1.00"},
		{"0.03", "0.03"},
		{"-12.3456", "-12.3456"},
		{"1000.0000000001", "1000.0000000001"},
		{"0.00000000009", "0.00"}, // truncated at scale 10
	}
	for _, tc := range cases {
		got, err := ParseNUMERIC(tc.in)
		if err != nil {
			t.Fatalf("ParseNUMERIC(%q): %v", tc.in, err)
		}
		if got.String() != tc.want {
			t.Fatalf("ParseNUMERIC(%q)=%s want %s", tc.in, got.String(), tc.want)
		}
	}
}

func TestParseNUMERICRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "abc", "1e5", "1.2.3", "--1", "0x10"} {
		if _, err := ParseNUMERIC(in); err == nil {
			t.Fatalf("ParseNUMERIC(%q) should fail", in)
		}
	}
}

func TestNUMERICScanAndValue(t *testing.T) {
	var n NUMERIC
	if err := n.Scan("0.0300000000"); err != nil {
		t.Fatalf("scan string: %v", err)
	}
	if n != FromFloat(0.03) {
		t.Fatalf("scan string = %s", n.String())
	}
	if err := n.Scan([]byte("42.5")); err != nil {
		t.Fatalf("scan bytes: %v", err)
	}
	if n.String() != "42.50" {
		t.Fatalf("scan bytes = %s", n.String())
	}
	if err := n.Scan(int64(7)); err != nil {
		t.Fatalf("scan int: %v", err)
	}
	if n.String() != "7.00" {
		t.Fatalf("scan int = %s", n.String())
	}

	v, err := FromFloat(1.25).Value()
	if err != nil {
		t.Fatalf("value: %v", err)
	}
	if v != "1.25" {
		t.Fatalf("value=%v", v)
	}
}

func TestNUMERICJSON(t *testing.T) {
	type payload struct {
		Price NUMERIC `json:"price_per_1m"`
	}
	raw, err := json.Marshal(payload{Price: FromFloat(10.5)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(raw) != `{"price_per_1m":10.5}` {
		t.Fatalf("json=%s", raw)
	}
	var back payload
	if err := json.Unmarshal([]byte(`{"price_per_1m":0.03}`), &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Price != FromFloat(0.03) {
		t.Fatalf("roundtrip=%s", back.Price.String())
	}
}

func TestFromFloatRoundsHalfUp(t *testing.T) {
	cases := map[float64]NUMERIC{
		0.00015:  1500000,
		0.1:      1000000000,
		-0.00015: -1500000,
		1.25:     12500000000,
	}
	for in, want := range cases {
		if got := FromFloat(in); got != want {
			t.Fatalf("FromFloat(%v)=%d want %d (%s)", in, got, want, got.String())
		}
	}
}
