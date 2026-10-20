package k3s

import (
	"strings"
	"testing"
)

func TestReleaseOrdering(t *testing.T) {
	// Ascending.
	tags := []string{
		"v1.33.4+k3s1",
		"v1.33.4+k3s2",
		"v1.33.10+k3s1",
		"v1.34.0-rc1+k3s1",
		"v1.34.0-rc2+k3s1",
		"v1.34.0+k3s1",
		"v1.34.1+k3s1",
	}
	for i := range tags {
		a, err := ParseRelease(tags[i])
		if err != nil {
			t.Fatal(err)
		}
		for j := range tags {
			b, _ := ParseRelease(tags[j])
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got := a.Compare(b); got != want {
				t.Errorf("%s vs %s = %d, want %d", tags[i], tags[j], got, want)
			}
		}
	}
}

func TestParseReleaseRejects(t *testing.T) {
	for _, tag := range []string{"", "v1.33.4", "1.33.4+k3s1", "v1.33+k3s1", "latest", "stable"} {
		if _, err := ParseRelease(tag); err == nil || !strings.Contains(err.Error(), "full release tag") {
			t.Errorf("ParseRelease(%q) = %v", tag, err)
		}
	}
}

func TestCheckUpgrade(t *testing.T) {
	for _, tt := range []struct {
		from, to string
		want     bool
		wantErr  string
	}{
		{from: "v1.33.4+k3s1", to: "v1.33.4+k3s1"},
		{from: "v1.33.4+k3s1", to: "v1.33.5+k3s1", want: true},
		{from: "v1.33.4+k3s1", to: "v1.33.4+k3s2", want: true},
		{from: "v1.33.4+k3s1", to: "v1.34.1+k3s1", want: true},
		{from: "v1.34.0-rc1+k3s1", to: "v1.34.0+k3s1", want: true},
		{from: "v1.34.1+k3s1", to: "v1.34.0+k3s1", wantErr: "doesn't support downgrades"},
		{from: "v1.34.1+k3s1", to: "v1.33.9+k3s1", wantErr: "doesn't support downgrades"},
		{from: "v1.32.1+k3s1", to: "v1.34.1+k3s1", wantErr: "upgrade to a v1.33 release first"},
		{from: "v1.33.4+k3s1", to: "v2.0.0+k3s1", wantErr: "would skip a minor version"},
		{from: "garbage", to: "v1.33.4+k3s1", wantErr: "installed version"},
		{from: "v1.33.4+k3s1", to: "v1.34", wantErr: "full release tag"},
	} {
		got, err := CheckUpgrade(tt.from, tt.to)
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("CheckUpgrade(%s, %s) = %v, want error containing %q", tt.from, tt.to, err, tt.wantErr)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("CheckUpgrade(%s, %s) = %v, %v; want %v", tt.from, tt.to, got, err, tt.want)
		}
	}
}
