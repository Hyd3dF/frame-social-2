package api

import "testing"

func TestCleanMessageBody(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "object replacement character", input: "Ne yapıyon aslan\uFFFC", want: "Ne yapıyon aslan"},
		{name: "zero width no break space", input: "mer\uFEFFhaba", want: "merhaba"},
		{name: "control characters", input: "a\u0000b\u0007c", want: "abc"},
		{name: "newlines and tabs", input: "  ilk\n\tikinci  ", want: "ilk\n\tikinci"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := cleanMessageBody(test.input); got != test.want {
				t.Fatalf("cleanMessageBody() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestNormalizeReactionsAggregatesEmojiAndKeepsOnlyOneOwnReaction(t *testing.T) {
	rows := []reactionView{
		{Emoji: "😂", Count: 1, Mine: true},
		{Emoji: "😂", Count: 1, Mine: false},
		{Emoji: "❤️", Count: 1, Mine: true}, // legacy duplicate from this account
		{Emoji: "❤️", Count: 1, Mine: false},
	}

	got := normalizeReactions(rows)
	if len(got) != 2 {
		t.Fatalf("normalizeReactions() returned %d reactions, want 2: %#v", len(got), got)
	}
	if got[0].Emoji != "😂" || got[0].Count != 2 || !got[0].Mine {
		t.Fatalf("first reaction = %#v, want mine 😂 with count 2", got[0])
	}
	if got[1].Emoji != "❤️" || got[1].Count != 1 || got[1].Mine {
		t.Fatalf("second reaction = %#v, want non-mine ❤️ with count 1", got[1])
	}
}
