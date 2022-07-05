package selfupdate

import (
	"context"
	"fmt"
	"regexp"
	"runtime"
	"strings"

	"github.com/blang/semver"
	"github.com/google/go-github/v30/github"
)

var reVersion = regexp.MustCompile(`\d+\.\d+\.\d+`)

func findAssetFromRelease(rel *github.RepositoryRelease,
	suffixes []string, targetVersion string, filters []*regexp.Regexp) (*github.ReleaseAsset, semver.Version, bool) {

	if targetVersion != "" && targetVersion != rel.GetTagName() {
		log.Println("Skip", rel.GetTagName(), "not matching to specified version", targetVersion)
		return nil, semver.Version{}, false
	}

	if targetVersion == "" && rel.GetDraft() {
		log.Println("Skip draft version", rel.GetTagName())
		return nil, semver.Version{}, false
	}
	if targetVersion == "" && rel.GetPrerelease() {
		log.Println("Skip pre-release version", rel.GetTagName())
		return nil, semver.Version{}, false
	}

	verText := rel.GetTagName()
	indices := reVersion.FindStringIndex(verText)
	if indices == nil {
		log.Println("Skip version not adopting semver", verText)
		return nil, semver.Version{}, false
	}
	if indices[0] > 0 {
		log.Println("Strip prefix of version", verText[:indices[0]], "from", verText)
		verText = verText[indices[0]:]
	}

	// If semver cannot parse the version text, it means that the text is not adopting
	// the semantic versioning. So it should be skipped.
	ver, err := semver.Make(verText)
	if err != nil {
		log.Println("Failed to parse a semantic version", verText)
		return nil, semver.Version{}, false
	}

	for _, asset := range rel.Assets {
		name := asset.GetName()
		if len(filters) > 0 {
			// if some filters are defined, match them: if any one matches, the asset is selected
			matched := false
			for _, filter := range filters {
				if filter.MatchString(name) {
					log.Println("Selected filtered asset", name)
					matched = true
					break
				}
				log.Printf("Skipping asset %q not matching filter %v\n", name, filter)
			}
			if !matched {
				continue
			}
		}

		for _, s := range suffixes {
			if strings.HasSuffix(name, s) { // require version, arch etc
				// default: assume single artifact
				return asset, ver, true
			}
		}
	}

	log.Println("No suitable asset was found in release", rel.GetTagName())
	return nil, semver.Version{}, false
}

func findValidationAsset(rel *github.RepositoryRelease, validationName string) (*github.ReleaseAsset, bool) {
	for _, asset := range rel.Assets {
		if asset.GetName() == validationName {
			return asset, true
		}
	}
	return nil, false
}

type releaseWithAssets struct {
	*github.RepositoryRelease
	*github.ReleaseAsset
	semver.Version
}

func findReleasesAndAssets(rels []*github.RepositoryRelease, targetVersion string, filters []*regexp.Regexp) (out []releaseWithAssets) {
	// Generate candidates
	suffixes := make([]string, 0, 2*7*2)
	for _, sep := range []rune{'_', '-'} {
		for _, ext := range []string{".zip", ".tar.gz", ".tgz", ".gzip", ".gz", ".tar.xz", ".xz", ""} {
			suffix := fmt.Sprintf("%s%c%s%s", runtime.GOOS, sep, runtime.GOARCH, ext)
			suffixes = append(suffixes, suffix)
			if runtime.GOOS == "windows" {
				suffix = fmt.Sprintf("%s%c%s.exe%s", runtime.GOOS, sep, runtime.GOARCH, ext)
				suffixes = append(suffixes, suffix)
			}
		}
	}

	// Find the latest version from the list of releases.
	// Returned list from GitHub API is in the order of the date when created.
	//   ref: https://github.com/rhysd/go-github-selfupdate/issues/11
	for _, rel := range rels {
		if a, v, ok := findAssetFromRelease(rel, suffixes, targetVersion, filters); ok {
			out = append(out, releaseWithAssets{RepositoryRelease: rel, ReleaseAsset: a, Version: v})
		}
	}

	return out
}

// DetectLatest tries to get the latest version of the repository on GitHub. 'slug' means 'owner/name' formatted string.
// It fetches releases information from GitHub API and find out the latest release with matching the tag names and asset names.
// Drafts and pre-releases are ignored. Assets would be suffixed by the OS name and the arch name such as 'foo_linux_amd64'
// where 'foo' is a command name. '-' can also be used as a separator. File can be compressed with zip, gzip, zxip, tar&zip or tar&zxip.
// So the asset can have a file extension for the corresponding compression format such as '.zip'.
// On Windows, '.exe' also can be contained such as 'foo_windows_amd64.exe.zip'.
func (up *Updater) DetectLatest(ctx context.Context, slug string) (release *Release, found bool, err error) {
	return up.DetectVersion(ctx, slug, "")
}

func (up *Updater) DetectVersions(ctx context.Context, slug string, version string) (releases []*Release, err error) {
	repo := strings.Split(slug, "/")
	if len(repo) != 2 || repo[0] == "" || repo[1] == "" {
		return nil, fmt.Errorf("Invalid slug format. It should be 'owner/name': %s", slug)
	}

	rels, res, err := up.api.Repositories.ListReleases(ctx, repo[0], repo[1], nil)
	if err != nil {
		log.Println("API returned an error response:", err)
		if res != nil && res.StatusCode == 404 {
			// 404 means repository not found or release not found. It's not an error here.
			err = nil
			log.Println("API returned 404. Repository or release not found")
		}
		return nil, err
	}

	for _, v := range findReleasesAndAssets(rels, version, up.filters) {
		url := v.ReleaseAsset.GetBrowserDownloadURL()
		log.Println("Successfully fetched the latest release. tag:", v.GetTagName(), ", name:", v.RepositoryRelease.GetName(), ", URL:", v.RepositoryRelease.GetURL(), ", Asset:", url)

		publishedAt := v.RepositoryRelease.GetPublishedAt().Time
		release := &Release{
			v.Version,
			url,
			v.ReleaseAsset.GetSize(),
			v.ReleaseAsset.GetID(),
			-1,
			v.RepositoryRelease.GetHTMLURL(),
			v.RepositoryRelease.GetBody(),
			v.RepositoryRelease.GetName(),
			&publishedAt,
			repo[0],
			repo[1],
		}
		if up.validator != nil {
			validationName := v.ReleaseAsset.GetName() + up.validator.Suffix()
			validationAsset, ok := findValidationAsset(v.RepositoryRelease, validationName)
			if !ok {
				log.Printf("Failed finding validation file %q", validationName)
				continue
			}
			release.ValidationAssetID = validationAsset.GetID()
		}
		releases = append(releases, release)
	}
	return releases, nil
}

// DetectVersion tries to get the given version of the repository on Github. `slug` means `owner/name` formatted string.
// And version indicates the required version.
func (up *Updater) DetectVersion(ctx context.Context, slug string, version string) (release *Release, found bool, err error) {
	rs, err := up.DetectVersions(ctx, slug, version)
	if err != nil {
		return nil, false, err
	}
	for _, v := range rs {
		if release == nil || v.Version.GTE(release.Version) {
			release = v
			found = true
		}
	}
	return
}

// DetectLatest detects the latest release of the slug (owner/repo).
// This function is a shortcut version of updater.DetectLatest() method.
func DetectLatest(ctx context.Context, slug string) (*Release, bool, error) {
	return DefaultUpdater(ctx).DetectLatest(ctx, slug)
}

// DetectVersion detects the given release of the slug (owner/repo) from its version.
func DetectVersion(ctx context.Context, slug string, version string) (*Release, bool, error) {
	return DefaultUpdater(ctx).DetectVersion(ctx, slug, version)
}
