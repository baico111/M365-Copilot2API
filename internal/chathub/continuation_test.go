package chathub

import (
	"strings"
	"testing"
)

func TestLooksLengthTruncated(t *testing.T) {
	long := strings.Repeat("测", continueMinChars)
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"short text never triggers", "结果如下：没有标点但是很短", false},
		{"ends with period", long + "。", false},
		{"ends with english period", long + ".", false},
		{"ends with exclamation", long + "！", false},
		{"ends with closing backtick inline code", long + "`", false},
		{"ends with closed fence", "```\ncode\n```", false},
		{"ends mid-sentence chinese char", long, true},
		{"ends with fullwidth comma", long + "，", true},
		{"ends with ascii digit", long + "3", true},
		{"ends with colon is considered finished", long + "：", false},
		{"ends with bracket", long + "）", false},
		{"ends newline trimmed then punctuation", long + "。\n\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := looksLengthTruncated(tc.text); got != tc.want {
				t.Fatalf("looksLengthTruncated=%v want %v", got, tc.want)
			}
		})
	}
}

func TestAdoptContinuation(t *testing.T) {
	if adoptContinuation("") {
		t.Fatal("empty continuation must not be adopted")
	}
	if adoptContinuation("  以上内容已经完整。  ") {
		t.Fatal("short closing note must not be adopted")
	}
	if !adoptContinuation("x" + strings.Repeat("补", continueAdoptMinChars)) {
		t.Fatal("substantive continuation must be adopted")
	}
	if !adoptContinuation(strings.Repeat("截", continueMinChars)) {
		t.Fatal("short-but-still-truncated continuation must be adopted")
	}
}
