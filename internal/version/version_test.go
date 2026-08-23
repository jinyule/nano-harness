package version

import "testing"

func TestInfoString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		info Info
		want string
	}{
		{name: "development", info: Info{Version: "dev"}, want: "nano-harness dev"},
		{
			name: "release",
			info: Info{Version: "v1.2.3", Commit: "abc123", Date: "2026-08-23T00:00:00Z"},
			want: "nano-harness v1.2.3 (commit abc123, built 2026-08-23T00:00:00Z)",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := test.info.String(); got != test.want {
				t.Fatalf("Info.String() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestCurrent(t *testing.T) {
	originalVersion, originalCommit, originalDate := Version, Commit, Date
	t.Cleanup(func() {
		Version, Commit, Date = originalVersion, originalCommit, originalDate
	})

	Version, Commit, Date = "v2.0.0", "deadbeef", "2026-08-23T01:02:03Z"
	want := Info{Version: Version, Commit: Commit, Date: Date}
	if got := Current(); got != want {
		t.Fatalf("Current() = %#v, want %#v", got, want)
	}
}
