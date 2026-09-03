package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// Client Skill packages: deterministic, complete, and never installed by us.

func readZip(t *testing.T, archive []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	out := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %s: %v", f.Name, err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		out[f.Name] = string(b)
	}
	return out
}

func zipNames(t *testing.T, archive []byte) []string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	out := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		out = append(out, f.Name)
	}
	return out
}

func clientFixture(t *testing.T, f *skillFixture) (skill, skillVersion) {
	t.Helper()
	sk, v := f.makeSkill(t, f.memberID, "Browser driver", skillDeliveryClient,
		"Run the driver with the target URL, then read the console log.", func(in *skillDraftInput) {
			triggers := []string{"browser", "screenshot"}
			in.Triggers = &triggers
			// Deliberately in a NON-alphabetical order, so the sort has
			// something to do.
			pkg := skillPackageMetadata{
				Entrypoint: "scripts/drive.sh",
				Scripts: []skillPackedFile{
					{Name: "drive.sh", Content: "#!/bin/sh\necho driving \"$1\"\n"},
					{Name: "assert.sh", Content: "#!/bin/sh\ntest -n \"$1\"\n"},
				},
				Assets: []skillPackedFile{{Name: "selectors.json", Content: `{"login":"#login"}`}},
			}
			in.Package = &pkg
			deps := []skillDependency{{Type: skillDepCapability, ID: "shell", Required: true}}
			in.Dependencies = &deps
			refs := []skillReference{{Kind: "url", Target: "https://example.com/driver", Label: "Driver docs"}}
			in.References = &refs
		})
	return f.publish(t, sk, v)
}

// The structure the PRD asks for, and the manifest that identifies exactly what
// somebody installed.
func TestClientPackageHasTheExpectedStructure(t *testing.T) {
	f := newSkillFixture(t)
	sk, v := clientFixture(t, f)

	archive, manifest, files, err := buildSkillPackage(sk, v)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	contents := readZip(t, archive)
	for _, want := range []string{
		"browser-driver/SKILL.md",
		"browser-driver/manifest.json",
		"browser-driver/references/references.md",
		"browser-driver/scripts/assert.sh",
		"browser-driver/scripts/drive.sh",
		"browser-driver/assets/selectors.json",
	} {
		if _, ok := contents[want]; !ok {
			t.Errorf("the package has no %s (it has %v)", want, zipNames(t, archive))
		}
	}
	if len(files) != len(contents) {
		t.Errorf("the reported file list has %d entries, the archive has %d", len(files), len(contents))
	}

	// SKILL.md carries the front matter a host-native skill loader reads, plus
	// the instructions and what the skill needs.
	skillMD := contents["browser-driver/SKILL.md"]
	for _, want := range []string{
		"name: browser-driver", "version: 1", "content_hash: " + v.ContentHash,
		"Run the driver with the target URL", "capability `shell`", "(required)",
	} {
		if !strings.Contains(skillMD, want) {
			t.Errorf("SKILL.md is missing %q:\n%s", want, skillMD)
		}
	}

	var stored skillManifest
	if err := json.Unmarshal([]byte(contents["browser-driver/manifest.json"]), &stored); err != nil {
		t.Fatalf("manifest is not JSON: %v", err)
	}
	if stored.SkillID != sk.ID || stored.Slug != "browser-driver" || stored.Version != 1 {
		t.Errorf("manifest identity is wrong: %+v", stored)
	}
	if stored.ContentHash != v.ContentHash {
		t.Errorf("manifest hash %s, want %s", shortHash(stored.ContentHash), shortHash(v.ContentHash))
	}
	if stored.DeliveryMode != skillDeliveryClient {
		t.Errorf("manifest delivery mode %q", stored.DeliveryMode)
	}
	if len(stored.Dependencies) != 1 || stored.Dependencies[0].ID != "shell" {
		t.Errorf("the manifest does not carry the dependencies: %+v", stored.Dependencies)
	}
	if manifest.Slug != stored.Slug {
		t.Error("the returned manifest and the one in the archive disagree")
	}
	// The references file says out loud that following a reference does not
	// make what it points at trusted.
	refs := contents["browser-driver/references/references.md"]
	if !strings.Contains(refs, "untrusted") {
		t.Errorf("references.md does not say a reference's target stays untrusted:\n%s", refs)
	}
}

// Determinism, which is the property the whole "compare what you installed
// against what was approved" story rests on. Built twice, the bytes must be
// identical — so no timestamps, no map iteration order, no default compression
// that could change under a Go upgrade.
func TestClientPackageIsByteForByteDeterministic(t *testing.T) {
	f := newSkillFixture(t)
	sk, v := clientFixture(t, f)

	first, _, _, err := buildSkillPackage(sk, v)
	if err != nil {
		t.Fatalf("first build: %v", err)
	}
	second, _, _, err := buildSkillPackage(sk, v)
	if err != nil {
		t.Fatalf("second build: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("two builds of the same immutable version differ (%d vs %d bytes) — the archive hash would then be meaningless",
			len(first), len(second))
	}
	if archiveHash(first) != archiveHash(second) {
		t.Fatal("the archive hashes differ")
	}

	// Reordering the input files must not change the bytes either: the
	// normalizer sorts them, and zip entry order is part of the archive.
	shuffled := v
	shuffled.Package = normalizePackage(skillPackageMetadata{
		Entrypoint: v.Package.Entrypoint,
		Scripts: []skillPackedFile{
			{Name: "drive.sh", Content: "#!/bin/sh\necho driving \"$1\"\n"},
			{Name: "assert.sh", Content: "#!/bin/sh\ntest -n \"$1\"\n"},
		},
		Assets: v.Package.Assets,
	})
	third, _, _, err := buildSkillPackage(sk, shuffled)
	if err != nil {
		t.Fatalf("third build: %v", err)
	}
	if !bytes.Equal(first, third) {
		t.Error("the same files in a different order produced different bytes")
	}
}

// A different version must produce a different archive, or the hash certifies
// nothing.
func TestADifferentVersionProducesADifferentPackage(t *testing.T) {
	f := newSkillFixture(t)
	sk, v := clientFixture(t, f)
	first, _, _, err := buildSkillPackage(sk, v)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	changed := v
	changed.Version = 2
	changed.Instructions = "Run the driver, then take a screenshot."
	changed.ContentHash = skillContentHash(sk.Slug, changed)
	second, _, _, err := buildSkillPackage(sk, changed)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("two different versions produced identical archives")
	}
}

// A remote skill has no package, and asking for one is a refusal rather than an
// empty zip somebody would try to install.
func TestARemoteSkillHasNoPackage(t *testing.T) {
	f := newSkillFixture(t)
	sk, v := f.makeSkill(t, f.memberID, "Remote only", skillDeliveryRemote, "Just read this.", nil)
	sk, approved := f.publish(t, sk, v)
	if _, _, _, err := buildSkillPackage(sk, approved); err == nil {
		t.Fatal("a Remote Skill produced a package")
	}
}

// The HTTP download: the real route, the real headers, and the audit row that
// says a package left the workspace.
func TestPackageDownloadServesTheArchiveAndAuditsIt(t *testing.T) {
	f := newSkillFixture(t)
	sk, v := clientFixture(t, f)

	rec := f.call(t, "GET", "/api/skills/"+sk.ID+"/versions/1/package", f.memberCookie, "")
	if rec.Code != 200 {
		t.Fatalf("download: %d %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/zip" {
		t.Errorf("content type %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "browser-driver-v1.zip") {
		t.Errorf("content disposition %q — a folder of files called package.zip helps nobody", cd)
	}
	if h := rec.Header().Get("X-Skill-Content-Hash"); h != v.ContentHash {
		t.Errorf("the response does not carry the content hash: %q", h)
	}
	contents := readZip(t, rec.Body.Bytes())
	if _, ok := contents["browser-driver/SKILL.md"]; !ok {
		t.Errorf("the served archive has no SKILL.md: %v", zipNames(t, rec.Body.Bytes()))
	}
	entries, err := f.s.skillAudit(sk.ID, skillAuditFilter{Action: skillActionPackageDownloaded})
	if err != nil || len(entries) != 1 {
		t.Fatalf("`package_downloaded` audit rows: %d (%v), want 1", len(entries), err)
	}
	if entries[0].ActorUserID != f.memberID || entries[0].ActorType != "human" {
		t.Errorf("the audit row does not identify the downloader: %+v", entries[0])
	}

	// An agent's token may download too — that is the point of a client skill.
	bearer := f.token(t, f.memberID, "read")
	rec = f.callWithToken(t, "GET", "/api/skills/"+sk.ID+"/versions/1/package", bearer, "")
	if rec.Code != 200 {
		t.Fatalf("download with a read token: %d %s", rec.Code, rec.Body.String())
	}
	entries, _ = f.s.skillAudit(sk.ID, skillAuditFilter{Action: skillActionPackageDownloaded})
	if len(entries) != 2 {
		t.Fatalf("%d download rows after two downloads", len(entries))
	}
	if entries[0].ActorType != "agent" {
		t.Errorf("the agent's download was recorded as %q", entries[0].ActorType)
	}

	// An outsider gets nothing.
	rec = f.call(t, "GET", "/api/skills/"+sk.ID+"/versions/1/package", f.outsiderCk, "")
	if rec.Code != 404 {
		t.Errorf("an outsider's download: %d, want 404", rec.Code)
	}
}

// An unapproved version has no package. A draft's scripts must not be
// downloadable — that would be executable content nobody reviewed.
func TestOnlyAnApprovedVersionHasADownload(t *testing.T) {
	f := newSkillFixture(t)
	sk, draft := f.makeSkill(t, f.memberID, "Unreviewed driver", skillDeliveryClient, "Run it.",
		func(in *skillDraftInput) {
			pkg := skillPackageMetadata{Scripts: []skillPackedFile{{Name: "evil.sh", Content: "rm -rf /"}}}
			in.Package = &pkg
		})
	rec := f.call(t, "GET", "/api/skills/"+sk.ID+"/versions/1/package", f.memberCookie, "")
	if rec.Code == 200 {
		t.Fatalf("a DRAFT client skill's package was served — the script in it was never reviewed:\n%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "skill_version_not_approved") {
		t.Errorf("the refusal reads %s — it should carry the machine-readable reason", rec.Body.String())
	}
	_ = draft
}

// A package file name may not climb out of the package directory.
func TestPackageFileNamesCannotEscape(t *testing.T) {
	f := newSkillFixture(t)
	name, description, delivery, instructions := "Escape", "d", skillDeliveryClient, "x"
	scope := skillScope{WorkspaceID: f.ws}
	pkg := skillPackageMetadata{Scripts: []skillPackedFile{{Name: "../../etc/cron.d/pwn", Content: "x"}}}
	if _, _, err := f.s.createSkill(f.userOf(t, f.memberID), f.ws, skillDraftInput{
		Name: &name, Description: &description, DeliveryMode: &delivery,
		Instructions: &instructions, Scope: &scope, Package: &pkg,
	}); err == nil {
		t.Fatal("a package file named ../../etc/cron.d/pwn was accepted")
	}
	// A leading slash is normalized away rather than refused: it is a common
	// typo, not an attack, and the result is unambiguous.
	if got := normalizePackedFiles([]skillPackedFile{{Name: "/scripts/x.sh"}}); len(got) != 1 || got[0].Name != "scripts/x.sh" {
		t.Errorf("a leading slash was not normalized: %+v", got)
	}
}

// A description with YAML-significant characters must not break the front
// matter — otherwise a host's skill loader rejects the whole file.
func TestPackageFrontMatterSurvivesAwkwardDescriptions(t *testing.T) {
	f := newSkillFixture(t)
	sk, v := f.makeSkill(t, f.memberID, "Awkward", skillDeliveryClient, "Do it.", nil)
	sk, approved := f.publish(t, sk, v)
	approved.Description = "Reads: config: {a: b} # and more\nacross two lines"

	md := clientSkillMarkdown(sk, approved)
	header := strings.SplitN(md, "---\n", 3)
	if len(header) < 3 {
		t.Fatalf("no front matter:\n%s", md)
	}
	// Four lines: name, description, version, content_hash. A newline that
	// leaked out of the description would make it five or more, and a host's
	// skill loader would reject the file.
	if strings.Count(header[1], "\n") != 4 {
		t.Errorf("the front matter has %d lines, want 4 — a newline in the description broke it:\n%s",
			strings.Count(header[1], "\n"), header[1])
	}
	if !strings.Contains(header[1], "description: 'Reads: config:") {
		t.Errorf("the description was not quoted:\n%s", header[1])
	}
}
