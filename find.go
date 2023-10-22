package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// A Finder returns a list of URLs making up a project's assets.
type Finder interface {
	Find() ([]string, error)
}

// A GithubRelease matches the Assets portion of Github's release API json.
type GithubRelease struct {
	Assets []struct {
		DownloadURL string `json:"browser_download_url"`
	} `json:"assets"`

	Prerelease bool      `json:"prerelease"`
	Tag        string    `json:"tag_name"`
	CreatedAt  time.Time `json:"created_at"`
}

// A GitlabRelease matches the Assets portion of Gitlab's release API json.
type GitlabRelease struct {
	Assets struct {
		Links []struct {
			DownloadURL string `json:"direct_asset_url"`
		} `json:"links"`
	} `json:"assets"`

	UpcomingRelease bool      `json:"upcoming_release"`
	Tag             string    `json:"tag_name"`
	CreatedAt       time.Time `json:"created_at"`
}

type GithubError struct {
	Code   int
	Status string
	Body   []byte
	Url    string
}

type githubErrResponse struct {
	Message string `json:"message"`
	Doc     string `json:"documentation_url"`
}

func (ge *GithubError) Error() string {
	var msg githubErrResponse
	json.Unmarshal(ge.Body, &msg)

	if ge.Code == http.StatusForbidden {
		return fmt.Sprintf("%s: %s: %s", ge.Status, msg.Message, msg.Doc)
	}
	return fmt.Sprintf("%s (URL: %s)", ge.Status, ge.Url)
}

type GitlabError struct {
	Code   int
	Status string
	Body   []byte
	Url    string
}

func (ge *GitlabError) Error() string {
	return fmt.Sprintf("%s (URL: %s)", ge.Status, ge.Url)
}

// A GithubAssetFinder finds assets for the given Repo at the given tag. Tags
// must be given as 'tag/<tag>'. Use 'latest' to get the latest release.
type GithubAssetFinder struct {
	Repo       string
	Tag        string
	Prerelease bool
	MinTime    time.Time // release must be after MinTime to be found
}

var ErrNoUpgrade = errors.New("requested release is not more recent than current version")

func (f *GithubAssetFinder) Find() ([]string, error) {
	if f.Prerelease && f.Tag == "latest" {
		tag, err := f.getLatestTag()
		if err != nil {
			return nil, err
		}
		f.Tag = fmt.Sprintf("tags/%s", tag)
	}

	// query github's API for this repo/tag pair.
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/%s", f.Repo, f.Tag)
	resp, err := Get(url)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(f.Tag, "tags/") && resp.StatusCode == http.StatusNotFound {
			return f.FindMatch()
		}
		return nil, &GithubError{
			Status: resp.Status,
			Code:   resp.StatusCode,
			Body:   body,
			Url:    url,
		}
	}

	// read and unmarshal the resulting json
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var release GithubRelease
	err = json.Unmarshal(body, &release)
	if err != nil {
		return nil, err
	}

	if release.CreatedAt.Before(f.MinTime) {
		return nil, ErrNoUpgrade
	}

	// accumulate all assets from the json into a slice
	assets := make([]string, 0, len(release.Assets))
	for _, a := range release.Assets {
		assets = append(assets, a.DownloadURL)
	}

	return assets, nil
}

func (f *GithubAssetFinder) FindMatch() ([]string, error) {
	tag := f.Tag[len("tags/"):]

	for page := 1; ; page++ {
		url := fmt.Sprintf("https://api.github.com/repos/%s/releases?page=%d", f.Repo, page)
		resp, err := Get(url)
		if err != nil {
			return nil, err
		}

		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return nil, err
			}
			return nil, &GithubError{
				Status: resp.Status,
				Code:   resp.StatusCode,
				Body:   body,
				Url:    url,
			}
		}

		// read and unmarshal the resulting json
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}

		var releases []GithubRelease
		err = json.Unmarshal(body, &releases)
		if err != nil {
			return nil, err
		}

		for _, r := range releases {
			if !f.Prerelease && r.Prerelease {
				continue
			}
			if strings.Contains(r.Tag, tag) && !r.CreatedAt.Before(f.MinTime) {
				// we have a winner
				assets := make([]string, 0, len(r.Assets))
				for _, a := range r.Assets {
					assets = append(assets, a.DownloadURL)
				}
				return assets, nil
			}
		}

		if len(releases) < 30 {
			break
		}
	}

	return nil, fmt.Errorf("no matching tag for '%s'", tag)
}

// finds the latest pre-release and returns the tag
func (f *GithubAssetFinder) getLatestTag() (string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases", f.Repo)
	resp, err := Get(url)
	if err != nil {
		return "", fmt.Errorf("pre-release finder: %w", err)
	}

	var releases []GithubRelease

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("pre-release finder: %w", err)
	}
	err = json.Unmarshal(body, &releases)
	if err != nil {
		return "", fmt.Errorf("pre-release finder: %w", err)
	}

	if len(releases) <= 0 {
		return "", fmt.Errorf("no releases found")
	}

	return releases[0].Tag, nil
}

// A GitlabAssetFinder finds assets for the given Repo at the given tag. Tags
// must be given as 'tag/<tag>'. Use 'latest' to get the latest release.
type GitlabAssetFinder struct {
	Repo       string
	Tag        string
	Prerelease bool
	MinTime    time.Time // release must be after MinTime to be found
}

func (f *GitlabAssetFinder) Find() ([]string, error) {
	if f.Prerelease && f.Tag == "latest" {
		tag, err := f.getLatestTag()
		if err != nil {
			return nil, err
		}
		f.Tag = fmt.Sprintf("tags/%s", tag)
	}

	var reqUrl string
	if f.Tag == "latest" {
		reqUrl = fmt.Sprintf("https://gitlab.com/api/v4/projects/%s/releases/permalink/latest", url.QueryEscape(f.Repo))
	} else {
		if t, ok := strings.CutPrefix(f.Tag, "tags/"); ok {
			reqUrl = fmt.Sprintf("https://gitlab.com/api/v4/projects/%s/releases/%s", url.QueryEscape(f.Repo), url.QueryEscape(t))
		} else {
			return nil, fmt.Errorf("asset finder: invalid tag format: %s", f.Tag)
		}
	}

	resp, err := Get(reqUrl)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(f.Tag, "tags/") && resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("no matching tag for '%s'", f.Tag[len("tags/"):])
		}
		return nil, &GitlabError{
			Status: resp.Status,
			Code:   resp.StatusCode,
			Body:   body,
			Url:    reqUrl,
		}
	}

	// read and unmarshal the resulting json
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var release GitlabRelease
	err = json.Unmarshal(body, &release)
	if err != nil {
		return nil, err
	}

	if release.CreatedAt.Before(f.MinTime) {
		return nil, ErrNoUpgrade
	}

	// accumulate all assets from the json into a slice
	assets := make([]string, 0, len(release.Assets.Links))
	for _, a := range release.Assets.Links {
		assets = append(assets, a.DownloadURL)
	}

	return assets, nil
}

// finds the latest pre-release and returns the tag
func (f *GitlabAssetFinder) getLatestTag() (string, error) {
	reqUrl := fmt.Sprintf("https://gitlab.com/api/v4/projects/%s/releases", url.QueryEscape(f.Repo))
	resp, err := Get(reqUrl)
	if err != nil {
		return "", fmt.Errorf("pre-release finder: %w", err)
	}

	var releases []GitlabRelease

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("pre-release finder: %w", err)
	}
	err = json.Unmarshal(body, &releases)
	if err != nil {
		return "", fmt.Errorf("pre-release finder: %w", err)
	}

	if len(releases) <= 0 {
		return "", fmt.Errorf("no releases found")
	}

	return releases[0].Tag, nil
}

// A DirectAssetFinder returns the embedded URL directly as the only asset.
type DirectAssetFinder struct {
	URL string
}

func (f *DirectAssetFinder) Find() ([]string, error) {
	return []string{f.URL}, nil
}

type GithubSourceFinder struct {
	Tool string
	Repo string
	Tag  string
}

func (f *GithubSourceFinder) Find() ([]string, error) {
	return []string{fmt.Sprintf("https://github.com/%s/tarball/%s/%s.tar.gz", f.Repo, f.Tag, f.Tool)}, nil
}

type GitlabSourceFinder struct {
	Tool string
	Repo string
	Tag  string
}

func (f *GitlabSourceFinder) Find() ([]string, error) {
	return []string{fmt.Sprintf("https://gitlab.com/%s/-/archive/%s/%s.tar.gz", f.Repo, f.Tag, f.Tool)}, nil
}
