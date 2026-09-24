package server

import "testing"

// TestFormatCredits 倍率原文归一化（去 "credits" 后缀与空白）。
func TestFormatCredits(t *testing.T) {
	cases := []struct{ in, want string }{
		{"x0.05 credits", "x0.05"},
		{"x0.11", "x0.11"},
		{"x0.00 credits", "x0.00"},
		{"  x2.20 credits  ", "x2.20"},
		{"", ""},
		{"credits", ""},
	}
	for _, c := range cases {
		if got := formatCredits(c.in); got != c.want {
			t.Errorf("formatCredits(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

