package main

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/sirupsen/logrus"

	prowconfig "k8s.io/test-infra/prow/config"
	"k8s.io/test-infra/prow/config/secret"
	"k8s.io/test-infra/prow/git/v2"
	"k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/prow/pluginhelp"
)

type githubClient interface {
	IsMember(org, user string) (bool, error)
	CreateComment(owner, repo string, number int, comment string) error
	GetPullRequest(org, repo string, number int) (*github.PullRequest, error)
}

var publicizeRe = regexp.MustCompile(`(?mi)^/publicize\s*$`)

func helpProvider(_ []prowconfig.OrgRepo) (*pluginhelp.PluginHelp, error) {
	pluginHelp := &pluginhelp.PluginHelp{
		Description: `The publicize plugin is used for merging and push the commit history to a configured upstream repository.`,
	}
	pluginHelp.AddCommand(pluginhelp.Command{
		Usage:       "/publicize",
		Description: "Merge the commit histories into the configured upstream repository",
		WhoCanUse:   "Members of the trusted organization for the repo.",
		Examples:    []string{"/publicize"},
	})
	return pluginHelp, nil
}

type server struct {
	githubTokenGenerator func() []byte

	gitName     string
	gitEmail    string
	githubLogin string
	githubHost  string

	config func() *Config

	ghc githubClient
	gc  git.ClientFactory

	secretAgent *secret.Agent

	dry bool
}

func (s *server) handleIssueComment(l *logrus.Entry, ic github.IssueCommentEvent) {
	if !publicizeRe.MatchString(ic.Comment.Body) {
		return
	}

	org := ic.Repo.Owner.Login
	repo := ic.Repo.Name
	num := ic.Issue.Number

	logger := logrus.WithFields(logrus.Fields{
		github.OrgLogField:  org,
		github.RepoLogField: repo,
		github.PrLogField:   num,
	})

	logger.Info("Publicize of PR has been requested.")

	pr, err := s.ghc.GetPullRequest(org, repo, num)
	if err != nil {
		logger.WithError(err).Warn("couldn't get pull request")
		s.createComment(ic, fmt.Sprintf("couldn't get pull request: %v", err), logger)
		return
	}
	baseBranch := pr.Base.Ref

	if err := s.checkPrerequisites(logger, pr, ic); err != nil {
		logger.WithError(err).Warn("error occurred while checking for prerequisites")
		s.createComment(ic, fmt.Sprintf("%v", err), logger)
		return
	}

	destOrgRepo := s.config().Repositories[fmt.Sprintf("%s/%s", org, repo)]
	destOrg := strings.Split(destOrgRepo, "/")[0]
	destRepo := strings.Split(destOrgRepo, "/")[1]

	sourceRemoteResolver := git.HttpResolver(func() (*url.URL, error) {
		return &url.URL{Scheme: "https", Host: s.githubHost, Path: fmt.Sprintf("%s/%s", org, repo)}, nil
	}, func() (login string, err error) { return s.githubLogin, nil }, s.githubTokenGenerator)

	logger.Infof("Trying to merge the PR to destination: %s/%s@%s", destOrg, destRepo, baseBranch)
	mergeMsg := fmt.Sprintf("merge history from %s/%s", org, repo)
	if err := s.mergeAndPushToRemote(destOrg, destRepo, sourceRemoteResolver, baseBranch, mergeMsg, s.dry); err != nil {
		logger.WithError(err).Warnf("couldn't merge the pull request and push to the destination: %v", err)
		s.createComment(ic, fmt.Sprintf("Publicize failed with error: %v", err), logger)
		return
	}

	destOrgRepoLink := fmt.Sprintf("https://%s/%s/tree/%s", s.githubHost, destOrgRepo, baseBranch)
	s.createComment(ic, fmt.Sprintf("A merge commit [%s/%s@%s](%s) was created in the upstream repository to publish this work.",
		destOrg, destRepo, baseBranch, destOrgRepoLink), logger)
}

func (s *server) checkPrerequisites(logger *logrus.Entry, pr *github.PullRequest, ic github.IssueCommentEvent) error {
	if !ic.Issue.IsPullRequest() {
		return errors.New("Publicize plugin can only be used in pull requests")
	}

	org := ic.Repo.Owner.Login
	commentAuthor := ic.Comment.User.Login

	// Only org members should be able to publicize a pull request.
	ok, err := s.ghc.IsMember(org, commentAuthor)
	if err != nil {
		return fmt.Errorf("couldn't check members: %w", err)
	}
	if !ok {
		return fmt.Errorf("only [%s](https://github.com/orgs/%s/people) org members may request publication of a private pull request", org, org)
	}

	if !pr.Merged {
		return errors.New("cannot publicize an unmerged pull request")
	}

	repo := ic.Repo.Name
	logger.Info("Searching for upstream repository")
	if _, ok := s.config().Repositories[fmt.Sprintf("%s/%s", org, repo)]; !ok {
		logger.Warn("There is no upstream repository configured for the current repository.")
		return fmt.Errorf("cannot publicize because there is no upstream repository configured for %s/%s", org, repo)
	}

	return nil
}

func (s *server) mergeAndPushToRemote(destOrg, destRepo string, sourceRemoteResolver func() (string, error), branch string, mergeMsg string, dry bool) error {
	repoClient, err := s.gc.ClientFor(destOrg, destRepo)
	if err != nil {
		return fmt.Errorf("couldn't create repoclient for repository %s/%s: %w", destOrg, destRepo, err)
	}

	defer func() {
		if err := repoClient.Clean(); err != nil {
			logrus.WithError(err).Error("couldn't clean temporary repo folder")
		}
	}()

	if err := repoClient.Checkout(branch); err != nil {
		return fmt.Errorf("couldn't checkout to branch %s: %w", branch, err)
	}

	if err := repoClient.FetchFromRemote(sourceRemoteResolver, branch); err != nil {
		return fmt.Errorf("couldn't fetch from the downstream repository: %w", err)
	}

	if err := repoClient.Config("user.name", s.gitName); err != nil {
		return fmt.Errorf("couldn't set config user.name=%s: %w", s.gitName, err)
	}

	if err := repoClient.Config("user.email", s.gitEmail); err != nil {
		return fmt.Errorf("couldn't set config user.name=%s: %w", s.gitEmail, err)
	}

	merged, err := repoClient.MergeWithStrategy("FETCH_HEAD", "merge", git.MergeOpt{CommitMessage: mergeMsg})
	if err != nil {
		return fmt.Errorf("couldn't merge %s/%s, merge --abort failed with reason: %w", destOrg, destRepo, err)
	}

	if !merged {
		return fmt.Errorf("couldn't merge %s/%s, possible because of a merge conflict", destOrg, destRepo)
	}

	if !dry {
		if err := repoClient.PushToCentral(branch, false); err != nil {
			return fmt.Errorf("couldn't push to upstream repository: %w", err)
		}
	}

	return nil
}

func (s *server) createComment(ic github.IssueCommentEvent, message string, logger *logrus.Entry) {
	censored := s.secretAgent.Censor([]byte(message))
	if err := s.ghc.CreateComment(ic.Repo.Owner.Login, ic.Repo.Name, ic.Issue.Number, fmt.Sprintf("@%s: %s", ic.Issue.User.Login, censored)); err != nil {
		logger.WithError(err).Warn("coulnd't create comment")
	}
}
