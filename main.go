// Copyright (c) 2025 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/bborbe/errors"
	libhttp "github.com/bborbe/http"
	"github.com/bborbe/log"
	libmetrics "github.com/bborbe/metrics"
	"github.com/bborbe/run"
	libsentry "github.com/bborbe/sentry"
	"github.com/bborbe/service"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/bborbe/a2a-worker-controller/pkg"
	"github.com/bborbe/a2a-worker-controller/pkg/factory"
)

func main() {
	app := &application{}
	os.Exit(service.Main(context.Background(), app, &app.SentryDSN, &app.SentryProxy))
}

type application struct {
	SentryDSN       string `required:"true"  arg:"sentry-dsn"        env:"SENTRY_DSN"        usage:"SentryDSN"                                             display:"length"`
	SentryProxy     string `required:"false" arg:"sentry-proxy"      env:"SENTRY_PROXY"      usage:"Sentry Proxy"`
	Listen          string `required:"true"  arg:"listen"            env:"LISTEN"            usage:"address to listen to"`
	Backend         string `required:"false" arg:"backend"           env:"BACKEND"           usage:"backend serving workers"                                                default:"docker"`
	A2APublicURL    string `required:"true"  arg:"a2a-public-url"    env:"A2A_PUBLIC_URL"    usage:"externally reachable A2A endpoint the card advertises"`
	A2AAgentName    string `required:"false" arg:"a2a-agent-name"    env:"A2A_AGENT_NAME"    usage:"agent name the Agent Card advertises"                                   default:"a2a-worker-controller"`
	BuildGitVersion string `required:"false" arg:"build-git-version" env:"BUILD_GIT_VERSION" usage:"Build Git version"                                                      default:"dev"`
	BuildGitCommit  string `required:"false" arg:"build-git-commit"  env:"BUILD_GIT_COMMIT"  usage:"Build Git commit hash"                                                  default:"none"`
}

func (a *application) Run(ctx context.Context, sentryClient libsentry.Client) error {
	libmetrics.NewBuildInfoMetrics().SetBuildInfo(a.BuildGitVersion, a.BuildGitCommit, nil)

	return service.Run(
		ctx,
		a.createHTTPServer(),
	)
}

func (a *application) createHTTPServer() run.Func {
	return func(ctx context.Context) error {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		backend, err := a.backend(ctx)
		if err != nil {
			return err
		}

		// The Docker and k8s implementations are wired in here by their own tasks; until
		// then the provider refuses the selected backend on use rather than serving a
		// request it cannot fulfil.
		provider := pkg.NewBackendProvider(nil, nil)
		executor := pkg.NewA2AExecutor(provider, backend)

		router := mux.NewRouter()
		router.Path("/healthz").Handler(factory.CreateHealthzHandler())
		// The controller has no external dependency to gate on, so readiness is
		// reported as soon as the listener is up.
		router.Path("/readiness").Handler(libhttp.NewPrintHandler("OK"))
		router.Path("/metrics").Handler(promhttp.Handler())
		router.Path("/setloglevel/{level}").
			Handler(log.NewSetLoglevelHandler(ctx, log.NewLogLevelSetter(2, 5*time.Minute)))
		router.Path("/gc").Handler(libhttp.NewGarbageCollectorHandler())
		router.Path(pkg.AgentCardPath).
			Handler(pkg.NewAgentCardHandler(pkg.NewAgentCard(pkg.AgentCardConfig{
				Name:      a.A2AAgentName,
				PublicURL: a.A2APublicURL,
			})))
		router.Path(pkg.A2APath).Handler(pkg.NewA2AHandler(executor))

		slog.Info("starting http server", "listen", a.Listen)
		return libhttp.NewServer(
			a.Listen,
			router,
		).Run(ctx)
	}
}

// backend resolves the backend the controller serves from its configured name. The closed
// set is the provider's own constants, so this branch names no backend of its own — it only
// decides which of them is selected, and the provider stays the single place that maps a
// name to an implementation.
func (a *application) backend(ctx context.Context) (pkg.Backend, error) {
	backend := pkg.Backend(a.Backend)
	switch backend {
	case pkg.BackendDocker, pkg.BackendKubernetes:
		return backend, nil
	default:
		return "", errors.Errorf(ctx, "unknown backend %q", a.Backend)
	}
}
