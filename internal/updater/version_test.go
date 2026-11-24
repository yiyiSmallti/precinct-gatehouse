package updater

import "testing"

func TestValidateVersion(t *testing.T) {
	for _, ok := range []string{"v0.1.0", "v0.1.2-rc.1", "baseline-20261123", "manual_abc1234"} {
		if err := ValidateVersion(ok); err != nil {
			t.Errorf("ValidateVersion(%q) unexpected error: %v", ok, err)
		}
	}
	for _, bad := range []string{"", ".", "..", "../evil", "a/b", "a b", "v1:2"} {
		if err := ValidateVersion(bad); err == nil {
			t.Errorf("ValidateVersion(%q) expected error, got nil", bad)
		}
	}
}

func TestCompareAndIsNewerVersion(t *testing.T) {
	cmpCases := []struct {
		a, b   string
		want   int
		wantOK bool
	}{
		{"v0.1.2", "v0.1.1", 1, true},
		{"0.2.0", "v0.1.9", 1, true},
		{"v0.1.1", "v0.1.1", 0, true},
		{"v0.1.1", "v0.1.2", -1, true},
		{"v0.1.2", "v0.1.2-rc.1", 1, true},
		{"v0.1.2-rc.1", "v0.1.2", -1, true},
		{"v0.1.2-rc.2", "v0.1.2-rc.1", 1, true},
		{"v0.1.2-1", "v0.1.2-alpha", -1, true},
		{"v0.1.2-alpha.1", "v0.1.2-alpha", 1, true},
		{"latest", "v0.1.1", 0, false},
	}
	for _, tc := range cmpCases {
		got, ok := CompareVersions(tc.a, tc.b)
		if ok != tc.wantOK || (ok && got != tc.want) {
			t.Errorf("CompareVersions(%q, %q) = (%d, %v), want (%d, %v)", tc.a, tc.b, got, ok, tc.want, tc.wantOK)
		}
	}

	newerCases := []struct {
		remote, current string
		want            bool
	}{
		{"v0.1.2", "v0.1.1", true},
		{"v0.1.1", "v0.1.1", false},
		{"v0.1.0", "v0.1.1", false},
		{"v0.1.2", "v0.1.2-rc.1", true},
		{"v0.1.0", "0.1.0-dev", true},
		{"manual-123", "dev", true},
		{"manual-123", "v0.1.2", false},
		{"", "v0.1.0", false},
	}
	for _, tc := range newerCases {
		if got := IsNewerVersion(tc.remote, tc.current); got != tc.want {
			t.Errorf("IsNewerVersion(%q, %q) = %v, want %v", tc.remote, tc.current, got, tc.want)
		}
	}
}
