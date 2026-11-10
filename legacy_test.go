package main

import "testing"

func TestLegacyRoundTrip(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"a", "$p$#"},
		{"ab", "$SB#"},
		{"abc", "MSK#"},
		{"abcd", "$$KMD_#S"},
		{"hello world", "YuHp$Hq&J(WPHFru"},
		{`{"a":1}`, "ep$$BMn%mYKi"},
		{"中文测试内容", "Q#PgQPyQQUSGycAFQG*tQ)PZ"},
	}
	for _, c := range cases {
		got := legacyEncode(c.in)
		if got != c.want {
			t.Errorf("encode(%q) = %q, want %q", c.in, got, c.want)
		}
		back, err := legacyDecode(got)
		if err != nil || back != c.in {
			t.Errorf("decode(%q) = %q, %v", got, back, err)
		}
	}
}
