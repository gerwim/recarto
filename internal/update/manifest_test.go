package update

import "testing"

const goldenManifest = `{
  "schema": 1,
  "package": "recarto",
  "latest": "1.4.0",
  "versions": [
    {"version":"1.4.0","released":"2026-06-07T12:00:00Z","artifacts":{
      "linux-amd64":{"path":"recarto/1.4.0/recarto-linux-amd64","sha256":"aa","size":10},
      "darwin-arm64":{"path":"recarto/1.4.0/recarto-darwin-arm64","sha256":"bb","size":11},
      "windows-amd64":{"path":"recarto/1.4.0/recarto-windows-amd64.exe","sha256":"cc","size":12}}},
    {"version":"1.3.0","released":"2026-06-01T12:00:00Z","artifacts":{
      "linux-amd64":{"path":"recarto/1.3.0/recarto-linux-amd64","sha256":"dd","size":9}}}
  ]
}`

func TestParseGolden(t *testing.T) {
	m, err := Parse([]byte(goldenManifest))
	if err != nil {
		t.Fatal(err)
	}
	if m.Package != "recarto" || m.Latest != "1.4.0" {
		t.Fatalf("bad header: %+v", m)
	}
	if len(m.Versions) != 2 {
		t.Fatalf("want 2 versions, got %d", len(m.Versions))
	}
	a, ok := m.Versions[0].Artifacts["linux-amd64"]
	if !ok || a.SHA256 != "aa" || a.Size != 10 {
		t.Fatalf("bad artifact: %+v", a)
	}
}

func TestParseRejectsUnknownSchema(t *testing.T) {
	if _, err := Parse([]byte(`{"schema":2,"package":"recarto","latest":"1.0.0","versions":[{"version":"1.0.0"}]}`)); err == nil {
		t.Fatal("expected error for unknown schema")
	}
}

func TestParseRejectsEmpty(t *testing.T) {
	if _, err := Parse([]byte(`{"schema":1,"package":"recarto","latest":"","versions":[]}`)); err == nil {
		t.Fatal("expected error for empty manifest")
	}
}

func TestParseRejectsNoVersions(t *testing.T) {
	if _, err := Parse([]byte(`{"schema":1,"package":"recarto","latest":"1.0.0","versions":[]}`)); err == nil {
		t.Fatal("expected error for empty versions list")
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	m, err := Parse([]byte(goldenManifest))
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	m2, err := Parse(b)
	if err != nil {
		t.Fatalf("re-parse failed: %v", err)
	}
	if m2.Latest != m.Latest || len(m2.Versions) != len(m.Versions) {
		t.Fatal("round-trip mismatch")
	}
	if m2.Versions[0].Artifacts["linux-amd64"].SHA256 != m.Versions[0].Artifacts["linux-amd64"].SHA256 {
		t.Fatal("artifact sha256 not preserved across round-trip")
	}
}

func TestPlatformKey(t *testing.T) {
	if got := platformKey("linux", "amd64"); got != "linux-amd64" {
		t.Fatalf("got %q", got)
	}
	if got := platformKey("windows", "amd64"); got != "windows-amd64" {
		t.Fatalf("got %q", got)
	}
}

func TestFindAndLatest(t *testing.T) {
	m, _ := Parse([]byte(goldenManifest))
	if _, ok := m.Find("1.3.0"); !ok {
		t.Fatal("1.3.0 should be found")
	}
	if _, ok := m.Find("9.9.9"); ok {
		t.Fatal("9.9.9 should not be found")
	}
	if _, ok := m.Find("v1.3.0"); !ok {
		t.Fatal("v1.3.0 (leading v) should be found")
	}
	r, ok := m.LatestRelease()
	if !ok || r.Version != "1.4.0" {
		t.Fatalf("latest release wrong: %+v", r)
	}
}

func TestArtifactForUnsupportedPlatform(t *testing.T) {
	m, _ := Parse([]byte(goldenManifest))
	r, _ := m.Find("1.3.0") // only has linux-amd64
	if _, ok := r.ArtifactFor("darwin-arm64"); ok {
		t.Fatal("darwin-arm64 should be missing in 1.3.0")
	}
	if _, ok := r.ArtifactFor("linux-amd64"); !ok {
		t.Fatal("linux-amd64 should be present in 1.3.0")
	}
}

func TestCompareVersions(t *testing.T) {
	if CompareVersions("1.4.0", "1.3.0") <= 0 {
		t.Fatal("1.4.0 > 1.3.0")
	}
	if CompareVersions("1.3.0", "1.3.0") != 0 {
		t.Fatal("equal")
	}
	if CompareVersions("v1.4.0", "1.4.0-dev") <= 0 {
		t.Fatal("release > prerelease")
	}
}

func TestSortedVersionsNewestFirst(t *testing.T) {
	m, _ := Parse([]byte(goldenManifest))
	m.Versions = []Release{{Version: "1.3.0"}, {Version: "1.4.0"}}
	got := m.SortedVersions()
	if got[0].Version != "1.4.0" || got[1].Version != "1.3.0" {
		t.Fatalf("not sorted newest-first: %+v", got)
	}
	if m.Versions[0].Version != "1.3.0" {
		t.Fatal("SortedVersions mutated the receiver")
	}
}

func TestIsReleaseBuild(t *testing.T) {
	cases := map[string]bool{"1.4.0": true, "v1.4.0": true, "0.1.0-dev": false, "1.4.0-dirty": false, "garbage": false}
	for v, want := range cases {
		if IsReleaseBuild(v) != want {
			t.Fatalf("IsReleaseBuild(%q)=%v want %v", v, IsReleaseBuild(v), want)
		}
	}
}
