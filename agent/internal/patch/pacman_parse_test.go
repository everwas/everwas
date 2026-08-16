package patch

import (
	"reflect"
	"testing"
)

// pacmanQuFixture is `pacman -Qu` output, including an IgnorePkg entry that
// must not be reported (the operator has told pacman never to take it, so
// it would sit in the console as permanently pending).
const pacmanQuFixture = `linux 6.9.1.arch1-1 -> 6.9.2.arch1-1
systemd 255.7-1 -> 256.1-1
vim 9.1.0436-1 -> 9.1.0448-1
nvidia 550.78-5 -> 550.90-1
firefox 126.0-1 -> 127.0-1 [ignored]
`

func TestParsePacmanQu(t *testing.T) {
	got := parsePacmanQu(pacmanQuFixture)
	want := []Update{
		{ID: "linux=6.9.2.arch1-1", Title: "linux 6.9.1.arch1-1 to 6.9.2.arch1-1",
			Kind: KindOther, Severity: SeverityUnknown, RebootLikely: true},
		{ID: "systemd=256.1-1", Title: "systemd 255.7-1 to 256.1-1",
			Kind: KindOther, Severity: SeverityUnknown, RebootLikely: true},
		{ID: "vim=9.1.0448-1", Title: "vim 9.1.0436-1 to 9.1.0448-1",
			Kind: KindOther, Severity: SeverityUnknown},
		{ID: "nvidia=550.90-1", Title: "nvidia 550.78-5 to 550.90-1",
			Kind: KindOther, Severity: SeverityUnknown, RebootLikely: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("\n got %+v\nwant %+v", got, want)
	}
}

func TestParsePacmanQuMalformed(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want int
	}{
		{"empty", "", 0},
		{"no arrow", "linux 6.9.1 6.9.2\n", 0},
		{"truncated", "linux 6.9.1 ->\n", 0},
		{"name only", "linux\n", 0},
		{"blank lines", "\n\n  \n", 0},
		{"trailing whitespace tolerated", "  vim 1.0-1 -> 1.1-1  \n", 1},
		{"error text from pacman", "error: no targets specified (use -h for help)\n", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := len(parsePacmanQu(tc.out)); got != tc.want {
				t.Errorf("parsed %d updates, want %d", got, tc.want)
			}
		})
	}
}

func TestParsePacmanQuery(t *testing.T) {
	got := parsePacmanQuery("linux 6.9.2.arch1-1\nvim 9.1.0448-1\nbroken\n")
	want := map[string]string{
		"linux": "6.9.2.arch1-1",
		"vim":   "9.1.0448-1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
