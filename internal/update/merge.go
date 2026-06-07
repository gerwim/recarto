package update

// MergeManifest returns a manifest with rel inserted (or replacing an existing
// entry of the same version), versions sorted newest-first, and Latest set to the
// highest version. A nil base starts a fresh recarto manifest. The input is not
// mutated.
func MergeManifest(base *Manifest, rel Release) *Manifest {
	out := &Manifest{Schema: SchemaVersion, Package: "recarto"}
	if base != nil {
		out.Package = base.Package
		for _, v := range base.Versions {
			if normalizeSemver(v.Version) != normalizeSemver(rel.Version) {
				out.Versions = append(out.Versions, v)
			}
		}
	}
	out.Versions = append(out.Versions, rel)
	out.Versions = out.SortedVersions()
	if len(out.Versions) > 0 {
		out.Latest = out.Versions[0].Version
	}
	return out
}
