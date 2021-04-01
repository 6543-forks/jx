package gits

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	errors2 "github.com/pkg/errors"

	"code.gitea.io/sdk/gitea"
	"github.com/google/go-github/v32/github"
	"github.com/jenkins-x/jx-logging/pkg/log"
	"github.com/jenkins-x/jx/v2/pkg/auth"
	"github.com/jenkins-x/jx/v2/pkg/util"
)

type GiteaProvider struct {
	Username string
	Client   *gitea.Client

	Server auth.AuthServer
	User   auth.UserAuth
	Git    Gitter
}

func NewGiteaProvider(server *auth.AuthServer, user *auth.UserAuth, git Gitter) (GitProvider, error) {
	client, err := gitea.NewClient(server.URL, gitea.SetToken(user.ApiToken))
	if err != nil {
		return nil, err
	}

	provider := GiteaProvider{
		Client:   client,
		Server:   *server,
		User:     *user,
		Username: user.Username,
		Git:      git,
	}

	return &provider, nil
}

func (p *GiteaProvider) ListOrganisations() ([]GitOrganisation, error) {
	answer := []GitOrganisation{}
	orgs, _, err := p.Client.ListMyOrgs(gitea.ListOrgsOptions{})
	if err != nil {
		return answer, err
	}

	for _, org := range orgs {
		name := org.UserName
		if name != "" {
			o := GitOrganisation{
				Login: name,
			}
			answer = append(answer, o)
		}
	}
	return answer, nil
}

func (p *GiteaProvider) ListRepositories(org string) ([]*GitRepository, error) {
	answer := []*GitRepository{}
	if org == "" {
		repos, _, err := p.Client.ListMyRepos(gitea.ListReposOptions{})
		if err != nil {
			return answer, err
		}
		for _, repo := range repos {
			answer = append(answer, toGiteaRepo(repo.Name, repo))
		}
		return answer, nil
	}
	repos, _, err := p.Client.ListOrgRepos(org, gitea.ListOrgReposOptions{})
	if err != nil {
		return answer, err
	}
	for _, repo := range repos {
		answer = append(answer, toGiteaRepo(repo.Name, repo))
	}
	return answer, nil
}

func (p *GiteaProvider) ListReleases(org string, name string) ([]*GitRelease, error) {
	owner := org
	if owner == "" {
		owner = p.Username
	}
	answer := []*GitRelease{}
	releases, _, err := p.Client.ListReleases(owner, name, gitea.ListReleasesOptions{})
	if err != nil {
		return answer, err
	}
	for _, r := range releases {
		answer = append(answer, toGiteaRelease(r))
	}
	return answer, nil
}

// GetRelease returns the release info for org, repo name and tag
func (p *GiteaProvider) GetRelease(org string, name string, tag string) (*GitRelease, error) {
	owner := org
	if owner == "" {
		owner = p.Username
	}
	release, _, err := p.Client.GetReleaseByTag(owner, name, tag)
	if err != nil {
		return nil, err
	}
	return toGiteaRelease(release), nil
}

func toGiteaRelease(release *gitea.Release) *GitRelease {
	totalDownloadCount := 0
	assets := make([]GitReleaseAsset, 0)
	for _, asset := range release.Attachments {
		totalDownloadCount = totalDownloadCount + int(asset.DownloadCount)
		assets = append(assets, GitReleaseAsset{
			ID:                 asset.ID,
			Name:               asset.Name,
			BrowserDownloadURL: asset.DownloadURL,
		})
	}
	return &GitRelease{
		ID:            release.ID,
		Name:          release.Title,
		TagName:       release.TagName,
		Body:          release.Note,
		PreRelease:    release.IsPrerelease,
		URL:           release.URL,
		HTMLURL:       release.URL,
		DownloadCount: totalDownloadCount,
		Assets:        &assets,
	}
}

func (p *GiteaProvider) CreateRepository(org string, name string, private bool) (*GitRepository, error) {
	var (
		repo    *gitea.Repository
		err     error
		options = gitea.CreateRepoOption{
			Name:    name,
			Private: private,
		}
	)

	if len(org) != 0 {
		repo, _, err = p.Client.CreateOrgRepo(org, options)
	}
	repo, _, err = p.Client.CreateRepo(options)

	if err != nil {
		return nil, fmt.Errorf("Failed to create repository %s/%s due to: %s", org, name, err)
	}
	return toGiteaRepo(name, repo), nil
}

func (p *GiteaProvider) GetRepository(org string, name string) (*GitRepository, error) {
	repo, _, err := p.Client.GetRepo(org, name)
	if err != nil {
		return nil, fmt.Errorf("Failed to get repository %s/%s due to: %s", org, name, err)
	}
	return toGiteaRepo(name, repo), nil
}

func (p *GiteaProvider) DeleteRepository(org string, name string) error {
	owner := org
	if owner == "" {
		owner = p.Username
	}

	if _, err := p.Client.DeleteRepo(owner, name); err != nil {
		return fmt.Errorf("Failed to delete repository %s/%s due to: %s", owner, name, err)
	}

	return nil
}

func toGiteaRepo(name string, repo *gitea.Repository) *GitRepository {
	return &GitRepository{
		AllowMergeCommit: true,
		Archived:         repo.Archived,
		CloneURL:         repo.CloneURL,
		Fork:             repo.Fork,
		HTMLURL:          repo.HTMLURL,
		HasIssues:        repo.HasIssues,
		HasProjects:      repo.HasProjects,
		HasWiki:          repo.HasWiki,
		ID:               repo.ID,
		Name:             name,
		OpenIssueCount:   repo.OpenIssues,
		Private:          repo.Private,
		SSHURL:           repo.SSHURL,
		Stars:            repo.Stars,
	}
}

func (p *GiteaProvider) ForkRepository(originalOrg string, name string, destinationOrg string) (*GitRepository, error) {
	repoConfig := gitea.CreateForkOption{
		Organization: &destinationOrg,
	}
	repo, _, err := p.Client.CreateFork(originalOrg, name, repoConfig)
	if err != nil {
		msg := ""
		if destinationOrg != "" {
			msg = fmt.Sprintf(" to %s", destinationOrg)
		}
		owner := destinationOrg
		if owner == "" {
			owner = p.Username
		}
		if strings.Contains(err.Error(), "try again later") {
			log.Logger().Warnf("Waiting for the fork of %s/%s to appear...", owner, name)
			// lets wait for the fork to occur...
			start := time.Now()
			deadline := start.Add(time.Minute)
			for {
				time.Sleep(5 * time.Second)
				repo, _, err = p.Client.GetRepo(owner, name)
				if repo != nil && err == nil {
					break
				}
				if time.Now().After(deadline) {
					return nil, fmt.Errorf("Gave up waiting for Repository %s/%s to appear: %s", owner, name, err)
				}
			}
		} else {
			return nil, fmt.Errorf("Failed to fork repository %s/%s%s due to: %s", originalOrg, name, msg, err)
		}
	}
	return toGiteaRepo(name, repo), nil
}

func (p *GiteaProvider) CreateWebHook(data *GitWebHookArguments) error {
	owner := data.Owner
	if owner == "" {
		owner = p.Username
	}
	repo := data.Repo.Name
	if repo == "" {
		return fmt.Errorf("Missing property Repo")
	}
	webhookUrl := data.URL
	if repo == "" {
		return fmt.Errorf("Missing property URL")
	}
	hooks, _, err := p.Client.ListRepoHooks(owner, repo, gitea.ListHooksOptions{})
	if err != nil {
		return err
	}
	for _, hook := range hooks {
		s := hook.Config["url"]
		if s == webhookUrl {
			log.Logger().Warnf("Already has a webhook registered for %s", webhookUrl)
			return nil
		}
	}
	config := map[string]string{
		"url":          webhookUrl,
		"content_type": "json",
	}
	if data.Secret != "" {
		config["secret"] = data.Secret
	}
	hook := gitea.CreateHookOption{
		Type:   "gitea",
		Config: config,
		Events: []string{"create", "push", "pull_request"},
		Active: true,
	}
	log.Logger().Infof("Creating Gitea webhook for %s/%s for url %s", util.ColorInfo(owner), util.ColorInfo(repo), util.ColorInfo(webhookUrl))
	_, _, err = p.Client.CreateRepoHook(owner, repo, hook)
	if err != nil {
		return fmt.Errorf("Failed to create webhook for %s/%s with %#v due to: %s", owner, repo, hook, err)
	}
	return err
}

func (p *GiteaProvider) ListWebHooks(owner string, repo string) ([]*GitWebHookArguments, error) {
	webHooks := []*GitWebHookArguments{}
	return webHooks, fmt.Errorf("ListWebHooks is currently not implemented for Gitea.")
	// p.Client.ListRepoHooks()
}

func (p *GiteaProvider) UpdateWebHook(data *GitWebHookArguments) error {
	return fmt.Errorf("UpdateWebHook is currently not implemented for Gitea.")
	// p.Client.EditRepoHook()
}

func (p *GiteaProvider) CreatePullRequest(data *GitPullRequestArguments) (*GitPullRequest, error) {
	owner := data.GitRepository.Organisation
	repo := data.GitRepository.Name
	title := data.Title
	body := data.Body
	head := data.Head
	base := data.Base
	config := gitea.CreatePullRequestOption{}
	if title != "" {
		config.Title = title
	}
	if body != "" {
		config.Body = body
	}
	if head != "" {
		config.Head = head
	}
	if base != "" {
		config.Base = base
	}
	pr, _, err := p.Client.CreatePullRequest(owner, repo, config)
	if err != nil {
		return nil, err
	}
	id := int(pr.Index)
	answer := &GitPullRequest{
		URL:    pr.HTMLURL,
		Number: &id,
		Owner:  data.GitRepository.Organisation,
		Repo:   data.GitRepository.Name,
	}
	if pr.Head != nil {
		answer.LastCommitSha = pr.Head.Sha
	}
	return answer, nil
}

// UpdatePullRequest updates pull request with number using data
func (p *GiteaProvider) UpdatePullRequest(data *GitPullRequestArguments, number int) (*GitPullRequest, error) {
	return nil, fmt.Errorf("UpdatePullRequest is currently not implemented for Gitea.")
}

func (p *GiteaProvider) UpdatePullRequestStatus(pr *GitPullRequest) error {
	if pr.Number == nil {
		return fmt.Errorf("Missing Number for GitPullRequest %#v", pr)
	}
	n := *pr.Number
	result, _, err := p.Client.GetPullRequest(pr.Owner, pr.Repo, int64(n))
	if err != nil {
		return fmt.Errorf("Could not find pull request for %s/%s #%d: %s", pr.Owner, pr.Repo, n, err)
	}
	p.updatePullRequest(pr, result)
	return nil
}

// updatePullRequest updates the pr with the data from Gitea
func (p *GiteaProvider) updatePullRequest(pr *GitPullRequest, source *gitea.PullRequest) {
	pr.Author = &GitUser{
		Login: source.Poster.UserName,
	}
	merged := source.HasMerged
	pr.Merged = &merged
	pr.Mergeable = &source.Mergeable
	pr.MergedAt = source.Merged
	pr.MergeCommitSHA = source.MergedCommitID
	pr.Title = source.Title
	pr.Body = source.Body
	stateText := string(source.State)
	pr.State = &stateText
	head := source.Head
	if head != nil {
		pr.LastCommitSha = head.Sha
	} else {
		pr.LastCommitSha = ""
	}
	/*
		TODO

		pr.ClosedAt = source.Closed
		pr.StatusesURL = source.StatusesURL
		pr.IssueURL = source.IssueURL
		pr.DiffURL = source.DiffURL
	*/
}

func (p *GiteaProvider) toPullRequest(owner string, repo string, pr *gitea.PullRequest) *GitPullRequest {
	id := int(pr.Index)
	answer := &GitPullRequest{
		URL:    pr.URL,
		Owner:  owner,
		Repo:   repo,
		Number: &id,
	}
	p.updatePullRequest(answer, pr)
	return answer
}

// ListOpenPullRequests lists the open pull requests
func (p *GiteaProvider) ListOpenPullRequests(owner string, repo string) ([]*GitPullRequest, error) {
	opt := gitea.ListPullRequestsOptions{}
	answer := []*GitPullRequest{}
	for {
		prs, _, err := p.Client.ListRepoPullRequests(owner, repo, opt)
		if err != nil {
			return answer, err
		}
		for _, pr := range prs {
			answer = append(answer, p.toPullRequest(owner, repo, pr))
		}
		if len(prs) < pageSize || len(prs) == 0 {
			break
		}
		opt.Page += 1
	}
	return answer, nil
}

func (p *GiteaProvider) GetPullRequest(owner string, repo *GitRepository, number int) (*GitPullRequest, error) {
	pr := &GitPullRequest{
		Owner:  owner,
		Repo:   repo.Name,
		Number: &number,
	}
	err := p.UpdatePullRequestStatus(pr)
	return pr, err
}

func (p *GiteaProvider) GetPullRequestCommits(owner string, repository *GitRepository, number int) ([]*GitCommit, error) {
	answer := []*GitCommit{}

	// TODO there does not seem to be any way to get a diff of commits
	// unless maybe checking out the repo (do we have access to a local copy?)
	// there is a pr.Base and pr.Head that might be able to compare to get
	// commits somehow, but does not look like anything through the api
	// https://github.com/go-gitea/gitea/issues/10918

	return answer, nil
}

func (p *GiteaProvider) GetIssue(org string, name string, number int) (*GitIssue, error) {
	i, resp, err := p.Client.GetIssue(org, name, int64(number))
	if err != nil {
		if resp != nil && resp.StatusCode == 404 {
			return nil, nil
		}
		return nil, err
	}
	return p.fromGiteaIssue(org, name, i)
}

func (p *GiteaProvider) IssueURL(org string, name string, number int, isPull bool) string {
	serverPrefix := p.Server.URL
	if strings.Index(serverPrefix, "://") < 0 {
		serverPrefix = "https://" + serverPrefix
	}
	path := "issues"
	if isPull {
		path = "pulls"
	}
	url := util.UrlJoin(serverPrefix, org, name, path, strconv.Itoa(number))
	return url
}

func (p *GiteaProvider) SearchIssues(org string, name string, filter string) ([]*GitIssue, error) {
	opts := gitea.ListIssueOption{
		KeyWord: filter,
	}
	return p.searchIssuesWithOptions(org, name, opts)
}

func (p *GiteaProvider) SearchIssuesClosedSince(org string, name string, t time.Time) ([]*GitIssue, error) {
	opts := gitea.ListIssueOption{
		State: gitea.StateClosed,
	}
	issues, err := p.searchIssuesWithOptions(org, name, opts)
	if err != nil {
		return issues, err
	}
	return FilterIssuesClosedSince(issues, t), nil
}

func (p *GiteaProvider) searchIssuesWithOptions(org string, name string, opts gitea.ListIssueOption) ([]*GitIssue, error) {
	opts.Page = 0
	answer := []*GitIssue{}
	issues, resp, err := p.Client.ListRepoIssues(org, name, opts)
	if err != nil {
		if resp != nil && resp.StatusCode == 404 {
			return answer, nil
		}
		return answer, err
	}
	for _, issue := range issues {
		i, err := p.fromGiteaIssue(org, name, issue)
		if err != nil {
			return answer, err
		}
		answer = append(answer, i)
	}
	return answer, nil
}

func (p *GiteaProvider) fromGiteaIssue(org string, name string, i *gitea.Issue) (*GitIssue, error) {
	state := string(i.State)
	labels := make([]GitLabel, 0, len(i.Labels))
	for _, label := range i.Labels {
		labels = append(labels, toGiteaLabel(label))
	}
	assignees := make([]GitUser, 0, len(i.Assignees))
	for _, assignee := range i.Assignees {
		assignees = append(assignees, *toGiteaUser(p.Server.URL, assignee))
	}
	number := int(i.ID)
	return &GitIssue{
		Number:        &number,
		URL:           p.IssueURL(org, name, number, false),
		State:         &state,
		Title:         i.Title,
		Body:          i.Body,
		IsPullRequest: i.PullRequest != nil,
		Labels:        labels,
		User:          toGiteaUser(p.Server.URL, i.Poster),
		Assignees:     assignees,
		CreatedAt:     &i.Created,
		UpdatedAt:     &i.Updated,
		ClosedAt:      i.Closed,
	}, nil
}

func (p *GiteaProvider) CreateIssue(owner string, repo string, issue *GitIssue) (*GitIssue, error) {
	config := gitea.CreateIssueOption{
		Title: issue.Title,
		Body:  issue.Body,
	}
	i, _, err := p.Client.CreateIssue(owner, repo, config)
	if err != nil {
		return nil, err
	}
	return p.fromGiteaIssue(owner, repo, i)
}

func toGiteaLabel(label *gitea.Label) GitLabel {
	return GitLabel{
		Name:  label.Name,
		Color: label.Color,
		URL:   label.URL,
	}
}

func toGiteaUser(serverURL string, user *gitea.User) *GitUser {
	return &GitUser{
		URL:       fmt.Sprintf("%s/%s", serverURL, user.UserName),
		Login:     user.UserName,
		Name:      user.FullName,
		Email:     user.Email,
		AvatarURL: user.AvatarURL,
	}
}

func (p *GiteaProvider) MergePullRequest(pr *GitPullRequest, message string) error {
	if pr.Number == nil {
		return fmt.Errorf("Missing Number for GitPullRequest %#v", pr)
	}
	_, _, err := p.Client.MergePullRequest(pr.Owner, pr.Repo, int64(*pr.Number), gitea.MergePullRequestOption{
		Style:   gitea.MergeStyleMerge,
		Title:   fmt.Sprintf("%s (#%d)", pr.Title, *pr.Number),
		Message: message,
	})
	return err
}

func (p *GiteaProvider) PullRequestLastCommitStatus(pr *GitPullRequest) (string, error) {
	ref := pr.LastCommitSha
	if ref == "" {
		return "", fmt.Errorf("Missing String for LastCommitSha %#v", pr)
	}
	results, _, err := p.Client.ListStatuses(pr.Owner, pr.Repo, ref, gitea.ListStatusesOption{})
	if err != nil {
		return "", err
	}
	for _, result := range results {
		text := string(result.State)
		if text != "" {
			return text, nil
		}
	}
	return "", fmt.Errorf("Could not find a status for repository %s/%s with ref %s", pr.Owner, pr.Repo, ref)
}

func (p *GiteaProvider) AddPRComment(pr *GitPullRequest, comment string) error {
	if pr.Number == nil {
		return fmt.Errorf("Missing Number for GitPullRequest %#v", pr)
	}
	n := *pr.Number
	prComment := gitea.CreateIssueCommentOption{
		Body: comment,
	}
	_, _, err := p.Client.CreateIssueComment(pr.Owner, pr.Repo, int64(n), prComment)
	return err
}

func (p *GiteaProvider) CreateIssueComment(owner string, repo string, number int, comment string) error {
	issueComment := gitea.CreateIssueCommentOption{
		Body: comment,
	}
	_, _, err := p.Client.CreateIssueComment(owner, repo, int64(number), issueComment)
	if err != nil {
		return err
	}
	return nil
}

func (p *GiteaProvider) ListCommitStatus(org string, repo string, sha string) ([]*GitRepoStatus, error) {
	answer := []*GitRepoStatus{}
	results, _, err := p.Client.ListStatuses(org, repo, sha, gitea.ListStatusesOption{})
	if err != nil {
		return answer, fmt.Errorf("Could not find a status for repository %s/%s with ref %s", org, repo, sha)
	}
	for _, result := range results {
		status := &GitRepoStatus{
			ID:          fmt.Sprint(result.ID),
			Context:     result.Context,
			URL:         result.URL,
			TargetURL:   result.TargetURL,
			State:       string(result.State),
			Description: result.Description,
		}
		answer = append(answer, status)
	}
	return answer, nil
}

func (b *GiteaProvider) UpdateCommitStatus(org string, repo string, sha string, status *GitRepoStatus) (*GitRepoStatus, error) {
	return &GitRepoStatus{}, fmt.Errorf("UpdateCommitStatus is currently not implemented for Gitea.")
}

func (p *GiteaProvider) RenameRepository(org string, name string, newName string) (*GitRepository, error) {
	return nil, fmt.Errorf("Rename of repositories is not supported for Gitea")
}

func (p *GiteaProvider) ValidateRepositoryName(org string, name string) error {
	_, resp, err := p.Client.GetRepo(org, name)
	if err == nil {
		return fmt.Errorf("Repository %s already exists", p.Git.RepoName(org, name))
	}
	if resp != nil && resp.StatusCode == 404 {
		return nil
	}
	return err
}

func (p *GiteaProvider) UpdateRelease(owner string, repo string, tag string, releaseInfo *GitRelease) error {
	release, resp, err := p.Client.GetReleaseByTag(owner, repo, tag)
	if err != nil && (resp == nil || resp.StatusCode != http.StatusNotFound) {
		return err
	}

	// lets populate the release
	if release == nil {
		createRelease := gitea.CreateReleaseOption{
			TagName:      releaseInfo.TagName,
			Title:        releaseInfo.Name,
			Note:         releaseInfo.Body,
			IsDraft:      false,
			IsPrerelease: false,
		}
		_, _, err = p.Client.CreateRelease(owner, repo, createRelease)
		return err
	} else {
		editRelease := gitea.EditReleaseOption{
			TagName:      release.TagName,
			Title:        release.Title,
			Note:         release.Note,
			IsDraft:      gitea.OptionalBool(false),
			IsPrerelease: gitea.OptionalBool(false),
		}
		if editRelease.Title == "" && releaseInfo.Name != "" {
			editRelease.Title = releaseInfo.Name
		}
		if editRelease.TagName == "" && releaseInfo.TagName != "" {
			editRelease.TagName = releaseInfo.TagName
		}
		if editRelease.Note == "" && releaseInfo.Body != "" {
			editRelease.Note = releaseInfo.Body
		}
		r2, _, err := p.Client.EditRelease(owner, repo, release.ID, editRelease)
		if err != nil {
			return err
		}
		if r2 != nil {
			releaseInfo.URL = r2.URL
		}
	}
	return err
}

// UpdateReleaseStatus updates the state (release/prerelease) of a release
func (p *GiteaProvider) UpdateReleaseStatus(owner string, repo string, tag string, releaseInfo *GitRelease) error {
	release, _, err := p.Client.GetReleaseByTag(owner, repo, tag)
	if err != nil {
		return err
	}

	editRelease := gitea.EditReleaseOption{
		TagName: release.TagName,
		Title:   release.Title,
		Note:    release.Note,
		IsDraft: gitea.OptionalBool(false),
	}

	if release.IsPrerelease != releaseInfo.PreRelease {
		editRelease.IsPrerelease = &releaseInfo.PreRelease
	}

	if _, _, err := p.Client.EditRelease(owner, repo, release.ID, editRelease); err != nil {
		return err
	}

	return nil
}

func (p *GiteaProvider) HasIssues() bool {
	return true
}

func (p *GiteaProvider) IsGitHub() bool {
	return false
}

func (p *GiteaProvider) IsGitea() bool {
	return true
}

func (p *GiteaProvider) IsBitbucketCloud() bool {
	return false
}

func (p *GiteaProvider) IsBitbucketServer() bool {
	return false
}

func (p *GiteaProvider) IsGerrit() bool {
	return false
}

func (p *GiteaProvider) Kind() string {
	return "gitea"
}

func (p *GiteaProvider) JenkinsWebHookPath(gitURL string, secret string) string {
	return "/gitea-webhook/post"
}

func GiteaAccessTokenURL(url string) string {
	return util.UrlJoin(url, "/user/settings/applications")
}

func (p *GiteaProvider) Label() string {
	return p.Server.Label()
}

func (p *GiteaProvider) ServerURL() string {
	return p.Server.URL
}

func (p *GiteaProvider) BranchArchiveURL(org string, name string, branch string) string {
	return util.UrlJoin(p.ServerURL(), org, name, "archive", branch+".zip")
}

func (p *GiteaProvider) UserAuth() auth.UserAuth {
	return p.User
}

func (p *GiteaProvider) CurrentUsername() string {
	return p.Username
}

func (p *GiteaProvider) UserInfo(username string) *GitUser {
	user, _, err := p.Client.GetUserInfo(username)

	if err != nil {
		return nil
	}

	return &GitUser{
		Login:     user.UserName,
		Name:      user.FullName,
		AvatarURL: user.AvatarURL,
		Email:     user.Email,
		URL:       p.Server.URL + "/" + user.UserName,
	}
}

func (p *GiteaProvider) AddCollaborator(user string, organisation string, repo string) error {
	log.Logger().Infof("Automatically adding the pipeline user as a collaborator is currently not implemented for Gitea. Please add user: %v as a collaborator to this project.", user)
	return nil
}

func (p *GiteaProvider) ListInvitations() ([]*github.RepositoryInvitation, *github.Response, error) {
	log.Logger().Infof("Automatically adding the pipeline user as a collaborator is currently not implemented for Gitea.")
	return []*github.RepositoryInvitation{}, &github.Response{}, nil
}

func (p *GiteaProvider) AcceptInvitation(ID int64) (*github.Response, error) {
	log.Logger().Infof("Automatically adding the pipeline user as a collaborator is currently not implemented for Gitea.")
	return &github.Response{}, nil
}

func (p *GiteaProvider) GetContent(org string, name string, path string, ref string) (*GitFileContent, error) {
	return nil, fmt.Errorf("GetContent is currently not implemented for Gitea.")
}

// ShouldForkForPullReques treturns true if we should create a personal fork of this repository
// before creating a pull request
func (p *GiteaProvider) ShouldForkForPullRequest(originalOwner string, repoName string, username string) bool {
	return originalOwner != username
}

func (p *GiteaProvider) ListCommits(owner, repo string, opt *ListCommitsArguments) ([]*GitCommit, error) {
	return nil, fmt.Errorf("ListCommits is currently not implemented for Gitea.")
}

// AddLabelsToIssue adds labels to issues or pulls
func (p *GiteaProvider) AddLabelsToIssue(owner, repo string, number int, labels []string) error {
	return fmt.Errorf("AddLabelsToIssue is currently not implemented for Gitea.")
}

// GetLatestRelease fetches the latest release from the git provider for org and name
func (p *GiteaProvider) GetLatestRelease(org string, name string) (*GitRelease, error) {
	// TODO filter drafts & pre-releases
	releases, _, err := p.Client.ListReleases(org, name, gitea.ListReleasesOptions{
		ListOptions: gitea.ListOptions{
			Page:     1,
			PageSize: 1,
		},
	})
	if err != nil {
		return nil, errors2.Wrapf(err, "getting releases for %s/%s", org, name)
	}
	if len(releases) != 0 {
		return toGiteaRelease(releases[0]), nil
	}
	return nil, err
}

// UploadReleaseAsset will upload an asset to org/repo to a release with id, giving it a name, it will return the release asset from the git provider
func (p *GiteaProvider) UploadReleaseAsset(org string, repo string, id int64, name string, asset *os.File) (*GitReleaseAsset, error) {
	a, _, err := p.Client.CreateReleaseAttachment(org, repo, id, asset, name)
	if a == nil || err != nil {
		return nil, err
	}
	return &GitReleaseAsset{
		ID:                 a.ID,
		BrowserDownloadURL: a.DownloadURL,
		Name:               a.Name,
	}, nil
}

// GetBranch returns the branch information for an owner/repo, including the commit at the tip
func (p *GiteaProvider) GetBranch(owner string, repo string, branch string) (*GitBranch, error) {
	b, _, err := p.Client.GetRepoBranch(owner, repo, branch)
	if b == nil || err != nil {
		return nil, err
	}
	return &GitBranch{
		Name: b.Name,
		Commit: &GitCommit{
			SHA:     b.Commit.ID,
			Message: b.Commit.Message,
			Author: &GitUser{
				URL:       p.Server.URL + "/" + b.Commit.Author.UserName,
				Login:     b.Commit.Author.UserName,
				Name:      b.Commit.Author.Name,
				Email:     b.Commit.Author.Email,
				AvatarURL: p.Server.URL + "/user/avatar/" + b.Commit.Author.UserName + "/-1",
			},
			Committer: &GitUser{
				URL:       p.Server.URL + "/" + b.Commit.Committer.UserName,
				Login:     b.Commit.Committer.UserName,
				Name:      b.Commit.Committer.Name,
				Email:     b.Commit.Committer.Email,
				AvatarURL: p.Server.URL + "/user/avatar/" + b.Commit.Committer.UserName + "/-1",
			},
			URL:    fmt.Sprintf("%s/%s/%s/commit/%s", p.Server.URL, owner, repo, b.Commit.ID),
			Branch: b.Name,
		},
		Protected: b.Protected,
	}, nil
}

// GetProjects returns all the git projects in owner/repo
func (p *GiteaProvider) GetProjects(owner string, repo string) ([]GitProject, error) {
	return nil, nil
}

//ConfigureFeatures sets specific features as enabled or disabled for owner/repo
func (p *GiteaProvider) ConfigureFeatures(owner string, repo string, issues *bool, projects *bool, wikis *bool) (*GitRepository, error) {
	return nil, nil
}

// IsWikiEnabled returns true if a wiki is enabled for owner/repo
func (p *GiteaProvider) IsWikiEnabled(owner string, repo string) (bool, error) {
	r, _, err := p.Client.GetRepo(owner, repo)
	if r == nil || err != nil {
		return false, err
	}
	return r.HasWiki, nil
}
