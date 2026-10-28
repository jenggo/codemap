package initcmd

import (
	"strings"
	"testing"
)

// insertTip must append the bullet inside the existing Tips section, before the
// next heading, and create the section when the document has none.
func TestInsertTip(t *testing.T) {
	tip := "`search` only returns exported symbols"
	bullet := "- " + tip

	cases := []struct {
		name       string
		in         string
		wantBefore string
		wantAfter  string
	}{
		{
			name:       "no tips section",
			in:         "# Title\n\nbody\n",
			wantBefore: "- Qualified names use `package/path.SymbolName` format",
		},
		{
			name:       "tips in middle",
			in:         "# T\n\n## Tips\n\n- old\n\n## Next\n\nbody",
			wantBefore: "- old",
			wantAfter:  "## Next",
		},
		{
			name:       "tips at end",
			in:         "# T\n\n## Tips\n\n- old\n",
			wantBefore: "- old",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(insertTip(strings.Split(tc.in, "\n"), tip), "\n")
			if !strings.Contains(got, bullet) {
				t.Fatalf("bullet missing from:\n%s", got)
			}
			if tc.wantBefore != "" {
				i := strings.Index(got, tc.wantBefore)
				j := strings.Index(got, bullet)
				if i < 0 || j < i {
					t.Errorf("%q must come before the bullet:\n%s", tc.wantBefore, got)
				}
			}
			if tc.wantAfter != "" {
				j := strings.Index(got, bullet)
				k := strings.Index(got, tc.wantAfter)
				if k < j {
					t.Errorf("bullet must come before %q:\n%s", tc.wantAfter, got)
				}
			}
		})
	}
}
