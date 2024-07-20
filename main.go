package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"

	"github.com/gofri/go-github-ratelimit/github_ratelimit"
	"github.com/google/go-github/v63/github"
	"github.com/m-mizutani/goerr"
	"golang.org/x/sync/errgroup"
	"gopkg.in/yaml.v3"
)

func getGitHubToken(ctx context.Context) (string, error) {
	if token, ok := os.LookupEnv("GITHUB_TOKEN"); ok {
		return strings.TrimSpace(token), nil
	}
	if token, ok := os.LookupEnv("OCTOCOV_GITHUB_TOKEN"); ok {
		return strings.TrimSpace(token), nil
	}

	if _, err := exec.LookPath("gh"); err != nil {
		return "", goerr.Wrap(err, "failed look path for `gh` command")
	}

	if err := exec.CommandContext(ctx, "gh", "auth", "status").Run(); err != nil {
		return "", goerr.Wrap(err, "failed exec for `gh auth status`")
	}

	res, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
	if err != nil {
		return "", goerr.Wrap(err, "failed exec for `gh auth token`")
	}
	return strings.TrimSpace(string(res)), nil
}

func canonicalUsername(ctx context.Context, client *github.Client, username string) (string, error) {
	if owner, ok := os.LookupEnv("GITHUB_REPOSITORY_OWNER"); ok {
		return strings.TrimSpace(owner), nil
	}

	user, _, err := client.Users.Get(ctx, strings.TrimSpace(username))
	if err != nil {
		return "", goerr.Wrap(err, "failed get user")
	}

	return user.GetLogin(), nil
}

func getPublicRepos(ctx context.Context, client *github.Client, username string) ([]*github.Repository, error) {
	publicRepos := []*github.Repository{}
	opts := &github.RepositoryListByUserOptions{}

	for {
		repos, resp, err := client.Repositories.ListByUser(ctx, username, opts)
		if err != nil {
			return nil, goerr.Wrap(err, "failed list by user repositories")
		}

		for _, repo := range repos {
			if repo.GetPrivate() {
				continue
			}
			if repo.GetArchived() {
				continue
			}
			if repo.GetFork() {
				continue
			}
			publicRepos = append(publicRepos, repo)
		}

		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	return publicRepos, nil
}

func getOctocovRepos(ctx context.Context, client *github.Client, repos []*github.Repository) ([]*github.Repository, error) {
	octocovRepos := []*github.Repository{}
	eg, ctx := errgroup.WithContext(ctx)
	mu := sync.Mutex{}
	re := regexp.MustCompile(`^GET [^\s]+: 404 Not Found`)

	for _, repo := range repos {
		eg.Go(func() error {
			_, _, _, err := client.Repositories.GetContents(ctx, repo.GetOwner().GetLogin(), repo.GetName(), ".octocov.yml", nil)
			if err != nil {
				if re.MatchString(err.Error()) {
					return nil
				} else {
					return goerr.Wrap(err, "failed get repository contents")
				}
			}

			mu.Lock()
			defer mu.Unlock()

			octocovRepos = append(octocovRepos, repo)

			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, goerr.Wrap(err, "failed errgroup")
	}

	return octocovRepos, nil
}

func rewriteOctocovYaml(repos []*github.Repository, dryRun bool) error {
	octocovYaml, err := os.ReadFile(".octocov.yml")
	if err != nil {
		return goerr.Wrap(err, "failed open .octocov.yml")
	}

	octocovMap := make(map[interface{}]interface{})
	if err := yaml.Unmarshal(octocovYaml, octocovMap); err != nil {
		return goerr.Wrap(err, "failed unmarshal .octocov.yml")
	}

	dataStores := make([]string, 0, len(repos))
	for _, repo := range repos {
		dataStores = append(dataStores, fmt.Sprintf("artifact://%s", repo.GetFullName()))
	}
	octocovMap["central"].(map[string]interface{})["reports"].(map[string]interface{})["datastores"] = dataStores

	rewriteYaml, err := yaml.Marshal(octocovMap)
	if err != nil {
		return goerr.Wrap(err, "failed marshal octocov map")
	}

	slog.Info("rewrite octocov", slog.Any("data", string(rewriteYaml)))
	if !dryRun {
		err := os.WriteFile(".octocov.yml", rewriteYaml, 0644) // #nosec G306
		if err != nil {
			return goerr.Wrap(err, "failed write .octocov.yml")
		}
	}

	return nil
}

func main() {
	var rawUsername string
	var logLevel string
	var dryRun bool
	flag.StringVar(&rawUsername, "username", "", "github username")
	flag.StringVar(&logLevel, "log-level", "info", "log level (debug,info,warn,error)")
	flag.BoolVar(&dryRun, "dry-run", false, ".octocov.yml rewrite dry-run")
	flag.Parse()

	slogOpts := &slog.HandlerOptions{}
	switch logLevel {
	case "debug":
		slogOpts.Level = slog.LevelDebug
	case "info":
		slogOpts.Level = slog.LevelInfo
	case "warn":
		slogOpts.Level = slog.LevelWarn
	case "error":
		slogOpts.Level = slog.LevelError
	default:
		slog.Error("failed log level", slog.Any("error", fmt.Errorf("unknown log level: %s", logLevel)))
		os.Exit(1)
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, slogOpts)))

	ctx := context.Background()

	token, err := getGitHubToken(ctx)
	if err != nil {
		slog.Error("failed get github token", slog.Any("error", err))
		os.Exit(1)
	}

	rateLimitter, err := github_ratelimit.NewRateLimitWaiterClient(
		nil,
		github_ratelimit.WithLimitDetectedCallback(func(callbackCtx *github_ratelimit.CallbackContext) {
			slog.Info(
				"detected rate limit",
				slog.String("sleep_until", callbackCtx.SleepUntil.String()),
				slog.String("total_sleep_time", callbackCtx.TotalSleepTime.String()),
			)
		}),
	)
	if err != nil {
		slog.Error("failed github ratelimit client", slog.Any("error", err))
		os.Exit(1)
	}

	client := github.NewClient(rateLimitter).WithAuthToken(token)

	username, err := canonicalUsername(ctx, client, rawUsername)
	if err != nil {
		slog.Error("failed canonical github username", slog.Any("error", err))
		os.Exit(1)
	}
	slog.Debug("github username", slog.String("username", username))

	publicRepos, err := getPublicRepos(ctx, client, username)
	if err != nil {
		slog.Error("failed get github public repositories", slog.Any("error", err))
		os.Exit(1)
	}
	slog.Debug("public repositories", slog.Any("repos", publicRepos))

	octocovRepos, err := getOctocovRepos(ctx, client, publicRepos)
	if err != nil {
		slog.Error("failed get github octocov repositories", slog.Any("error", err))
		os.Exit(1)
	}
	slog.Debug("octocov repositories", slog.Any("repos", octocovRepos))

	if err := rewriteOctocovYaml(octocovRepos, dryRun); err != nil {
		slog.Error("failed rewrite octocov yaml", slog.Any("error", err))
		os.Exit(1)
	}
}
