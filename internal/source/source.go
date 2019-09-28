// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package source constructs public URLs that link to the source files in a module. It
// can be used to build references to Go source code, or to any other files in a
// module.
//
// Of course, the module zip file contains all the files in the module. This
// package attempts to find the origin of the zip file, in a repository that is
// publicly readable, and constructs links to that repo. While a module zip file
// could in theory come from anywhere, including a non-public location, this
// package recognizes standard module path patterns and construct repository
// URLs from them, like the go command does.
package source

//
// Much of this code was adapted from
// https://go.googlesource.com/gddo/+/refs/heads/master/gosrc
// and
// https://go.googlesource.com/go/+/refs/heads/master/src/cmd/go/internal/get

// TODO(b/141769404): distinguish between vN as a branch vs. subdirectory, for N > 1.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/discovery/internal/derrors"
	"golang.org/x/discovery/internal/log"
	"golang.org/x/discovery/internal/stdlib"
)

// Info holds source information about a module, used to generate URLs referring
// to directories, files and lines.
type Info struct {
	// TODO(b/141771951): change the DB schema of versions to include this information
	RepoURL   string       // URL of repo containing module; exported for DB schema compatibility
	moduleDir string       // directory of module relative to repo root
	commit    string       // tag or ID of commit corresponding to version
	templates urlTemplates // for building URLs
}

// ModuleURL returns a URL for the home page of the module.
func (i *Info) ModuleURL() string {
	return i.DirectoryURL("")
}

// DirectoryURL returns a URL for a directory relative to the module's home directory.
func (i *Info) DirectoryURL(dir string) string {
	return strings.TrimSuffix(expand(i.templates.directory, map[string]string{
		"repo":   i.RepoURL,
		"commit": i.commit,
		"dir":    path.Join(i.moduleDir, dir),
	}), "/")
}

// FileURL returns a URL for a file whose pathname is relative to the module's home directory.
func (i *Info) FileURL(pathname string) string {
	return expand(i.templates.file, map[string]string{
		"repo":   i.RepoURL,
		"commit": i.commit,
		"file":   path.Join(i.moduleDir, pathname),
	})
}

// LineURL returns a URL referring to a line in a file relative to the module's home directory.
func (i *Info) LineURL(pathname string, line int) string {
	return expand(i.templates.line, map[string]string{
		"repo":   i.RepoURL,
		"commit": i.commit,
		"file":   path.Join(i.moduleDir, pathname),
		"line":   strconv.Itoa(line),
	})
}

// ModuleInfo determines the repository corresponding to the module path. It
// returns a URL to that repo, as well as the directory of the module relative
// to the repo root.
//
// ModuleInfo may fetch from arbitrary URLs, so it can be slow.
func ModuleInfo(ctx context.Context, modulePath, version string) (_ *Info, err error) {
	defer derrors.Wrap(&err, "source.ModuleInfo(ctx, %q, %q)", modulePath, version)

	return moduleInfo(ctx, http.DefaultClient, modulePath, version)
}

func moduleInfo(ctx context.Context, client *http.Client, modulePath, version string) (_ *Info, err error) {
	if modulePath == stdlib.ModulePath {
		commit, err := stdlib.TagForVersion(version)
		if err != nil {
			return nil, err
		}
		return &Info{
			RepoURL:   stdlib.GoSourceRepoURL,
			moduleDir: stdlib.Directory(version),
			commit:    commit,
			templates: githubURLTemplates,
		}, nil
	}
	repo, dir, templates, err := matchStatic(modulePath)
	if err != nil {
		return moduleInfoDynamic(ctx, client, modulePath, version)
	}
	return &Info{
		RepoURL:   "https://" + repo,
		moduleDir: dir,
		commit:    commitFromVersion(version, dir),
		templates: templates,
	}, nil
	// TODO(b/141770842): support launchpad.net, including the special case in cmd/go/internal/get/vcs.go.
}

// matchStatic matches its argument against a list of known patterns.
// It returns the repo name, directory and URL templates if there is a match.
func matchStatic(modulePathOrRepoURL string) (repo, dir string, _ urlTemplates, _ error) {
	for _, pat := range patterns {
		matches := pat.re.FindStringSubmatch(modulePathOrRepoURL)
		if matches == nil {
			continue
		}
		var repo string
		for i, n := range pat.re.SubexpNames() {
			if n == "repo" {
				repo = matches[i]
				break
			}
		}
		// The directory is everything after what the pattern matches.
		dir := modulePathOrRepoURL[len(matches[0]):]
		dir = strings.TrimPrefix(dir, "/")
		return repo, dir, pat.templates, nil
	}
	return "", "", urlTemplates{}, derrors.NotFound
}

// moduleInfoDynamic uses the go-import and go-source meta tags to construct an Info.
func moduleInfoDynamic(ctx context.Context, client *http.Client, modulePath, version string) (_ *Info, err error) {
	defer derrors.Wrap(&err, "source.moduleInfoDynamic(ctx, client, %q, %q)", modulePath, version)

	// Don't let requests to arbitrary URLs take too long.
	ctx, cancel := context.WithTimeout(ctx, 1*time.Minute)
	defer cancel()
	sourceMeta, err := fetchMeta(ctx, client, modulePath)
	if err != nil {
		return nil, err
	}
	// Don't check that the tag information at the repo root prefix is the same
	// as in the module path. It was done for us by the proxy and/or go command.
	// (This lets us merge information from the go-import and go-source tags.)

	// sourceMeta contains some information about where the module's source lives. But there
	// are some problems:
	// - We may only have a go-import tag, not a go-source tag, so we don't have URL templates for
	//   building URLs to files and directories.
	// - Even if we do have a go-source tag, its URL template format predates
	//   versioning, so the URL templates won't provide a way to specify a
	//   version or commit.
	//
	// We resolve these problems as follows:
	// 1. First look at the repo URL from the tag. If that matches a known hosting site, use the
	//    URL templates corresponding to that site and ignore whatever's in the tag.
	// 2. TODO(b/141847689): heuristically determine how to construct a URL template with a commit from the
	//    existing go-source template. For example, by replacing "master" with "{commit}".
	// We could also consider using the repo in the go-import tag instead of the one in the go-source tag,
	// if the former matches a known pattern but the latter does not.
	rurl, err := url.Parse(sourceMeta.repoURL)
	if err != nil {
		return nil, err
	}
	_, _, templates, err := matchStatic(path.Join(rurl.Hostname(), rurl.Path))
	if templates == (urlTemplates{}) {
		// Log this as an error so that we can notice it and possibly add it to our
		// list of static patterns.
		log.Errorf("no templates for repo URL %q from meta tag: err=%v", sourceMeta.repoURL, err)
	}
	dir := strings.TrimPrefix(strings.TrimPrefix(modulePath, sourceMeta.repoRootPrefix), "/")
	return &Info{
		RepoURL:   strings.TrimSuffix(sourceMeta.repoURL, "/"),
		moduleDir: dir,
		commit:    commitFromVersion(version, dir),
		templates: templates,
	}, nil
}

//  Patterns for determining repo and URL templates from module paths or repo URLs.
var patterns = []struct {
	re        *regexp.Regexp // matches path or repo; must have a group named "repo"
	templates urlTemplates
}{
	// Patterns known to the go command.
	{
		regexp.MustCompile(`^(?P<repo>github\.com/[a-z0-9A-Z_.\-]+/[a-z0-9A-Z_.\-]+)`),
		githubURLTemplates,
	},
	{
		regexp.MustCompile(`^(?P<repo>bitbucket\.org/[a-z0-9A-Z_.\-]+/[a-z0-9A-Z_.\-]+)`),
		urlTemplates{
			directory: "{repo}/src/{commit}/{dir}",
			file:      "{repo}/src/{commit}/{file}",
			line:      "{repo}/src/{commit}/{file}#lines-{line}",
		},
	},
	// Other patterns from cmd/go/internal/get/vcs.go, that we omit:
	// hub.jazz.net it no longer exists.
	// git.apache.org now redirects to github, and serves a go-import tag.
	// git.openstack.org has been rebranded.
	// chiselapp.com has no Go packages in godoc.org.

	// Patterns that match the general go command pattern, where they must have
	// a ".git" repo suffix in an import path. If matching a repo URL from a meta tag,
	// there is no ".git".
	{
		regexp.MustCompile(`^(?P<repo>[^.]+\.googlesource.com/[^.]+)(\.git|$)`),
		urlTemplates{
			directory: "{repo}/+/{commit}/{dir}",
			file:      "{repo}/+/{commit}/{file}",
			line:      "{repo}/+/{commit}/{file}#{line}",
		},
	},
	// General syntax for the go command. We can extract the repo and directory, but
	// we don't know the URL templates.
	// Must be last in this list.
	{
		regexp.MustCompile(`(?P<repo>([a-z0-9.\-]+\.)+[a-z0-9.\-]+(:[0-9]+)?(/~?[A-Za-z0-9_.\-]+)+?)\.(bzr|fossil|git|hg|svn)`),
		urlTemplates{},
	},
}

func init() {
	for _, p := range patterns {
		found := false
		for _, n := range p.re.SubexpNames() {
			if n == "repo" {
				found = true
				break
			}
		}
		if !found {
			panic(fmt.Sprintf("pattern %s missing <repo> group", p.re))
		}
	}
}

// urlTemplates describes how to build URLs from bits of source information.
type urlTemplates struct {
	directory string // URL template for a directory, with {repo}, {commit} and {dir}
	file      string // URL template for a file, with {repo}, {commit} and {file}
	line      string // URL template for a line, with {repo}, {commit}, {file} and {line}
}

var githubURLTemplates = urlTemplates{
	directory: "{repo}/tree/{commit}/{dir}",
	file:      "{repo}/blob/{commit}/{file}",
	line:      "{repo}/blob/{commit}/{file}#L{line}",
}

// commitFromVersion returns a string that refers to a commit corresponding to version.
// The string may be a tag, or it may be the hash or similar unique identifier of a commit.
// The second argument is the directory of the module relative to the repo root.
func commitFromVersion(version, dir string) string {
	// Commit for the module: either a sha for pseudoversions, or a tag.
	v := strings.TrimSuffix(version, "+incompatible")
	if isPseudoVersion(v) {
		// Use the commit hash at the end.
		return v[strings.LastIndex(v, "-")+1:]
	} else {
		commit := v
		// The tags for a nested module begin with the relative directory of the module.
		if dir != "" {
			commit = dir + "/" + commit
		}
		return commit
	}
}

// The following code copied from internal/etl/fetch.go:

var pseudoVersionRE = regexp.MustCompile(`^v[0-9]+\.(0\.0-|\d+\.\d+-([^+]*\.)?0\.)\d{14}-[A-Za-z0-9]+(\+incompatible)?$`)

// isPseudoVersion reports whether a valid version v is a pseudo-version.
// Modified from src/cmd/go/internal/modfetch.
func isPseudoVersion(v string) bool {
	return strings.Count(v, "-") >= 2 && pseudoVersionRE.MatchString(v)
}

// The following code copied from cmd/go/internal/get:

// expand rewrites s to replace {k} with match[k] for each key k in match.
func expand(s string, match map[string]string) string {
	// We want to replace each match exactly once, and the result of expansion
	// must not depend on the iteration order through the map.
	// A strings.Replacer has exactly the properties we're looking for.
	oldNew := make([]string, 0, 2*len(match))
	for k, v := range match {
		oldNew = append(oldNew, "{"+k+"}", v)
	}
	return strings.NewReplacer(oldNew...).Replace(s)
}