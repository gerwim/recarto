package update

import "testing"

func TestMergeManifestInsertsNewest(t *testing.T) {
	base := &Manifest{Schema: 1, Package: "recarto", Latest: "1.3.0", Versions: []Release{
		{Version: "1.3.0", Artifacts: map[string]Artifact{"linux-amd64": {Path: "p", SHA256: "x", Size: 1}}},
	}}
	rel := Release{Version: "1.4.0", Released: "2026-06-07T00:00:00Z",
		Artifacts: map[string]Artifact{"linux-amd64": {Path: "q", SHA256: "y", Size: 2}}}
	out := MergeManifest(base, rel)
	if out.Latest != "1.4.0" {
		t.Fatalf("latest not bumped: %s", out.Latest)
	}
	if out.Versions[0].Version != "1.4.0" {
		t.Fatalf("newest not first: %+v", out.Versions)
	}
	if len(out.Versions) != 2 {
		t.Fatalf("want 2 versions: %d", len(out.Versions))
	}
}

func TestMergeManifestReplacesSameVersion(t *testing.T) {
	base := &Manifest{Schema: 1, Package: "recarto", Latest: "1.4.0", Versions: []Release{
		{Version: "1.4.0", Artifacts: map[string]Artifact{"linux-amd64": {SHA256: "old"}}},
	}}
	rel := Release{Version: "1.4.0", Artifacts: map[string]Artifact{"linux-amd64": {SHA256: "new"}}}
	out := MergeManifest(base, rel)
	if len(out.Versions) != 1 || out.Versions[0].Artifacts["linux-amd64"].SHA256 != "new" {
		t.Fatalf("did not replace existing version: %+v", out.Versions)
	}
}

func TestMergeManifestNilBase(t *testing.T) {
	rel := Release{Version: "1.0.0", Artifacts: map[string]Artifact{"linux-amd64": {SHA256: "z"}}}
	out := MergeManifest(nil, rel)
	if out.Schema != SchemaVersion || out.Package != "recarto" || out.Latest != "1.0.0" || len(out.Versions) != 1 {
		t.Fatalf("nil-base merge wrong: %+v", out)
	}
}
