package filter

import "testing"

func TestExcluderMatch(t *testing.T) {
	ex, err := NewExcluder([]string{"tmp/**", "*.log"}, []string{"^private/"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		path string
		want bool
	}{
		{path: "tmp/a.txt", want: true},
		{path: "a.log", want: true},
		{path: "private/secrets.txt", want: true},
		{path: "public/readme.md", want: false},
	}
	for _, tc := range cases {
		got, _ := ex.Match(tc.path)
		if got != tc.want {
			t.Fatalf("path %s got=%v want=%v", tc.path, got, tc.want)
		}
	}
}
