package server

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	"github.com/heptiolabs/healthcheck"
	"github.com/labstack/echo-contrib/prometheus"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/pkg/errors"
	"github.com/rs/zerolog/log"
	"github.com/spf13/viper"
	"github.com/zapier/kubechecks/pkg"
	"github.com/zapier/kubechecks/pkg/argo_client"
	"github.com/zapier/kubechecks/pkg/vcs_clients"
	"github.com/ziflex/lecho/v3"
)

const KubeChecksHooksPathPrefix = "/hooks"

var singleton *Server

type Server struct {
	cfg *pkg.ServerConfig
}

func NewServer(cfg *pkg.ServerConfig) *Server {
	singleton = &Server{cfg: cfg}
	return singleton
}

func GetServer() *Server {
	return singleton
}

func (s *Server) Start() {
	if err := s.buildVcsToArgoMap(); err != nil {
		log.Warn().Err(err).Msg("failed to build vcs app map from argo")
	}

	if err := s.ensureWebhooks(); err != nil {
		log.Warn().Err(err).Msg("failed to create webhooks")
	}

	e := echo.New()
	e.HideBanner = true
	e.Use(middleware.Recover())
	e.Logger = lecho.New(log.Logger)
	// Enable metrics middleware
	p := prometheus.NewPrometheus("kubechecks_echo", nil)
	p.Use(e)

	// add routes
	health := healthcheck.NewHandler()
	e.GET("/ready", echo.WrapHandler(health))
	e.GET("/live", echo.WrapHandler(health))

	hooksGroup := e.Group(s.hooksPrefix())

	ghHooks := NewVCSHookHandler(s.cfg)
	ghHooks.AttachHandlers(hooksGroup)

	if err := e.Start(":8080"); err != nil {
		log.Fatal().Err(err).Msg("could not start hooks server")
	}
}

func (s *Server) hooksPrefix() string {
	prefix := s.cfg.UrlPrefix
	url, err := url.JoinPath("/", prefix, KubeChecksHooksPathPrefix)
	if err != nil {
		log.Warn().Err(err).Msg(":whatintarnation:")
	}

	return strings.TrimSuffix(url, "/")
}

func (s *Server) ensureWebhooks() error {
	if !viper.GetBool("ensure-webhooks") {
		return nil
	}

	if !viper.GetBool("monitor-all-applications") {
		return errors.New("must enable 'monitor-all-applications' to create webhooks")
	}

	urlBase := viper.GetString("webhook-url-base")
	if urlBase == "" {
		return errors.New("must define 'webhook-url-base' to create webhooks")
	}

	fmt.Println("ensuring all webhooks are created correctly")

	ctx := context.TODO()
	vcsClient, _ := GetVCSClient()

	fullUrl, err := url.JoinPath(urlBase, s.hooksPrefix())
	if err != nil {
		return errors.Wrap(err, "failed to create a webhook url")
	}

	for repo := range s.cfg.VcsToArgoMap.VcsRepos {
		_, err := vcsClient.GetHookByUrl(ctx, repo, fullUrl)
		if err != nil && err != vcs_clients.ErrHookNotFound {
			println(fmt.Sprintf("failed to get hook for %s:", repo))
			println(err)
			continue
		}

		if err = vcsClient.CreateHook(ctx, repo, fullUrl, s.cfg.WebhookSecret); err != nil {
			println(fmt.Sprintf("failed to create hook for %s:", repo))
			println(err.Error())
		}
	}

	return nil
}

func (s *Server) buildVcsToArgoMap() error {
	if !viper.GetBool("monitor-all-applications") {
		return nil
	}

	ctx := context.TODO()

	result := pkg.VcsToArgoMap{
		VcsRepos: make(map[string][]v1alpha1.Application),
	}

	argoClient := argo_client.GetArgoClient()
	apps, err := argoClient.GetApplications(ctx)
	if err != nil {
		return errors.Wrap(err, "failed to list applications")
	}

	for _, app := range apps.Items {
		if app.Spec.Source == nil {
			continue
		}

		appsForRepo := result.VcsRepos[app.Spec.Source.RepoURL]
		appsForRepo = append(appsForRepo, app)
		result.VcsRepos[app.Spec.Source.RepoURL] = appsForRepo
	}

	s.cfg.VcsToArgoMap = result
	return nil
}
