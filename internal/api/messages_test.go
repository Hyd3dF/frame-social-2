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
