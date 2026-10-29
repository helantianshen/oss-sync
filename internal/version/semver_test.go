// 本文件验证影响发布选择的版本解析和优先级规则
package version

import "testing"

func TestParsePreservesPrereleaseAndBuild(t *testing.T) {
	parsed, err := Parse(" v2.10.3-rc.12+build.007 ")
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Major != 2 || parsed.Minor != 10 || parsed.Patch != 3 || parsed.Prerelease != "rc.12" || parsed.Build != "build.007" {
		t.Fatalf("version components lost: %+v", parsed)
	}
	if got := parsed.String(); got != "2.10.3-rc.12+build.007" {
		t.Fatalf("canonical version = %q", got)
	}
}

func TestParseRejectsAmbiguousReleaseVersions(t *testing.T) {
	for _, input := range []string{"", "v", "1.2", "01.2.3", "1.02.3", "1.2.03", "1.2.3-rc.01", "1.2.3-", "1.2.3+", "1.2.3-rc..1", "1.2.3+bad/path"} {
		t.Run(input, func(t *testing.T) {
			if _, err := Parse(input); err == nil {
				t.Fatalf("accepted invalid release version %q", input)
			}
		})
	}
}

func TestCompareReleasePrecedence(t *testing.T) {
	cases := []struct {
		name string
		a    string
		b    string
		want int
	}{
		{"major", "2.0.0", "1.99.99", 1},
		{"numeric minor", "1.10.0", "1.9.99", 1},
		{"numeric rc", "1.2.3-rc.10", "1.2.3-rc.2", 1},
		{"stable after rc", "1.2.3", "1.2.3-rc.10", 1},
		{"numeric before lexical", "1.2.3-1", "1.2.3-alpha", -1},
		{"shorter prerelease", "1.2.3-alpha", "1.2.3-alpha.1", -1},
		{"build ignored", "v1.2.3+build.1", "1.2.3+build.2", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Compare(tc.a, tc.b)
			if err != nil || got != tc.want {
				t.Fatalf("Compare(%q, %q) = %d, %v; want %d", tc.a, tc.b, got, err, tc.want)
			}
			reverse, err := Compare(tc.b, tc.a)
			if err != nil || reverse != -tc.want {
				t.Fatalf("reverse comparison = %d, %v; want %d", reverse, err, -tc.want)
			}
		})
	}
	for _, pair := range [][2]string{{"dev", "1.2.3"}, {"1.2.3", "1.2"}} {
		if _, err := Compare(pair[0], pair[1]); err == nil {
			t.Fatalf("invalid comparison %q treated as a valid ordering", pair)
		}
	}
}
