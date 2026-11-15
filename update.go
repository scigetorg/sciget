package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/mod/semver"
)

// httpsOnly is a CheckRedirect that refuses to leave https.
func httpsOnly(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return fmt.Errorf("refused a redirect to %s", req.URL.Redacted())
	}
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	return nil
}

// latestRelease returns the tag of the newest release, read from where
// releasesURL (the update_url setting, GitHub's ".../releases/latest" web
// page) redirects: ".../releases/tag/<tag>". The web page is used, not
// GitHub's API, whose limit of 60 requests an hour is shared by everyone
// behind one address. The tag's own page is not fetched.
func latestRelease(client *http.Client, releasesURL string) (string, error) {
	c := *client
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if err := httpsOnly(req, via); err != nil {
			return err
		}
		if strings.Contains(req.URL.Path, "/releases/tag/") || strings.HasSuffix(req.URL.Path, "/releases") {
			return http.ErrUseLastResponse
		}
		return nil
	}
	req, err := http.NewRequest(http.MethodGet, releasesURL, nil)
	if err != nil {
		return "", fmt.Errorf("checking for updates: %w", err)
	}
	req.Header.Set("User-Agent", "sciget/"+version)
	resp, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("checking for updates: %w", err)
	}
	defer resp.Body.Close()
	loc, err := resp.Location()
	switch {
	case err != nil || resp.StatusCode < 300 || resp.StatusCode > 399:
		return "", fmt.Errorf("checking for updates: %s returned %s, not a release", releasesURL, resp.Status)
	case strings.HasSuffix(loc.Path, "/releases"): // GitHub's answer when there are none
		return "", errors.New("checking for updates: no releases yet")
	}
	_, tag, _ := strings.Cut(loc.Path, "/releases/tag/")
	if tag, err = url.PathUnescape(tag); err != nil || tag == "" {
		return "", errors.New("checking for updates: the release has no tag")
	}
	return printable(tag), nil
}

// updateResult describes the running version against the latest release,
// for the TUI's navbar: "√ up to date", "↑ v0.2.0 available", or why it
// cannot say. Versions compare as semantic versions, with an optional "v",
// so a pre-release such as v0.1.0-rc1 is older than v0.1.0.
func updateResult(running, latest string) string {
	semantic := func(v string) string { return "v" + strings.TrimPrefix(v, "v") }
	switch {
	case running == "dev":
		return "development build"
	case !semver.IsValid(semantic(running)):
		return "unknown version " + running
	case !semver.IsValid(semantic(latest)):
		return "couldn't read the latest version"
	case semver.Compare(semantic(latest), semantic(running)) > 0:
		return "↑ " + latest + " available · see github.com/scigetorg/sciget/releases"
	}
	return "√ up to date"
}
