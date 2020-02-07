/*
Copyright 2020 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package internal

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/pkg/errors"
	"k8s.io/release/pkg/git"
	"k8s.io/release/pkg/log"
	"k8s.io/release/pkg/notes"
	noteopts "k8s.io/release/pkg/notes/options"
)

const relnoteScript = `
set -euo pipefail
tmp="$( mktemp )"
trap 'rm -f -- "${tmp}"' EXIT
%q --htmlize-md --preview --quiet --markdown-file="${tmp}" >&2
cat "${tmp}"
`

type ReleaseNoter struct {
	log.Mixin

	ReleaseToolsDir string
	K8sDir          string
	GithubToken     string

	CommandCreator CommandCreator
	RepoOpener     RepoOpener
}

func (r *ReleaseNoter) GetMarkdown() (string, error) {
	notes, err := r.relnotes()
	if err != nil {
		return "", errors.Wrapf(err, "gathering release notes")
	}

	prs := "### some pending PRs"
	builds := "### find a green build"

	return strings.Join([]string{notes, prs, builds}, "\n\n----\n\n"), nil
}

var releaseTagRE = regexp.MustCompile(`^v\d+\.\d+.\d+$`)

func filterReleaseTags(tags []string) []string {
	filtered := []string{}
	for _, tag := range tags {
		if releaseTagRE.MatchString(tag) {
			filtered = append(filtered, tag)
		}
	}
	return filtered
}

//counterfeiter:generate . Repo
type Repo interface {
	CurrentBranch() (branch string, err error)
	TagsForBranch(branch string) (tags []string, err error)
	Head() (hash string, err error)
}

type RepoOpener func(path string) (Repo, error)

var defaultRepoOpener RepoOpener = func(p string) (Repo, error) {
	return git.OpenRepo(p)
}

func (o RepoOpener) Open(path string) (Repo, error) {
	if o == nil {
		o = defaultRepoOpener
	}
	return o(path)
}

func (r *ReleaseNoter) relnotes() (string, error) {
	repo, err := r.RepoOpener.Open(r.K8sDir)
	if err != nil {
		return "", errors.Wrapf(err, "opening repo")
	}

	branch, err := repo.CurrentBranch()
	if err != nil {
		return "", errors.Wrapf(err, "getting current checked out branch")
	}

	tags, err := repo.TagsForBranch(branch)
	if err != nil {
		return "", errors.Wrapf(err, "getting tags on current branch")
	}
	releaseTags := filterReleaseTags(tags)
	if len(releaseTags) < 1 {
		return "", fmt.Errorf("could not find a release tag (%q) on the current branch", releaseTagRE)
	}
	newestPatchRelease := releaseTags[0]

	headSHA, err := repo.Head()
	if err != nil {
		return "", errors.Wrapf(err, "getting head of current branch")
	}

	opts := &noteopts.Options{
		DiscoverMode: noteopts.RevisionDiscoveryModeNONE,
		GithubOrg:    git.DefaultGithubOrg,
		GithubRepo:   git.DefaultGithubRepo,
		Pull:         false,
		RepoPath:     r.K8sDir,
		GithubToken:  r.GithubToken,
		StartRev:     newestPatchRelease,
		EndSHA:       headSHA,
		// Branch :  branch,
	}

	if err := opts.ValidateAndFinish(); err != nil {
		return "", errors.Wrapf(err, "finishing opts")
	}

	ctx := context.TODO()
	gatherer := notes.NewGatherer(ctx, opts)
	releaseNotes, history, err := gatherer.ListReleaseNotes()
	if err != nil {
		return "", errors.Wrapf(err, "listing release notes")
	}

	// Create the markdown
	doc, err := notes.CreateDocument(releaseNotes, history)
	if err != nil {
		return "", errors.Wrapf(err, "creating release note document")
	}

	markdown, err := notes.RenderMarkdown(
		doc, "", "",
		opts.StartRev, opts.EndRev,
	)
	if err != nil {
		return "", errors.Wrapf(err, "rendering release notes to markdown")
	}

	fmt.Fprintf(os.Stderr, "\n\n----\n%s\n----\n\n", markdown)

	return "", fmt.Errorf("some error")
	return markdown, nil
}

func (r *ReleaseNoter) getRelnotesMarkdown() (string, error) {
	binPath, err := filepath.Abs(filepath.Join(r.ReleaseToolsDir, "relnotes"))
	if err != nil {
		return "", fmt.Errorf("could not determine current working directory")
	}
	r.Logger().WithField("binpath", binPath).Debug("binpath set")

	cmd := r.CommandCreator.create(
		"bash", "-c", fmt.Sprintf(relnoteScript, binPath),
	)
	if cmd == nil {
		return "", fmt.Errorf("command is nil")
	}
	r.Logger().Debug("command created")

	cmd.SetDir(r.K8sDir)
	cmd.SetEnv([]string{
		"GITHUB_TOKEN=" + r.GithubToken,
	})

	r.Logger().WithField("workdir", r.K8sDir).Info("starting release notes gatherer ... this may take a while ...")

	s, eerr := cmdOutput(cmd)
	if eerr != nil {
		r.Logger().WithError(eerr).Debug("execing & getting output failed")
		r.Logger().WithField("error", eerr.FullError()).Trace("full exec error")
		return "", eerr
	}
	return s, nil
}
