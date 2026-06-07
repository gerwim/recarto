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
