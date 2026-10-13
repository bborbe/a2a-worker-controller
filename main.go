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
	SentryDSN       string `required:"true"  arg:"sentry-dsn"            env:"SENTRY_DSN"            usage:"SentryDSN"                                                                           display:"length"`
	SentryProxy     string `required:"false" arg:"sentry-proxy"          env:"SENTRY_PROXY"          usage:"Sentry Proxy"`
	Listen          string `required:"true"  arg:"listen"                env:"LISTEN"                usage:"address to listen to"`
	Backend         string `required:"false" arg:"backend"               env:"BACKEND"               usage:"backend serving workers"                                                                              default:"docker"`
	WorkerImage     string `required:"true"  arg:"worker-image"          env:"WORKER_IMAGE"          usage:"image every created worker container runs"`
	A2APublicURL    string `required:"true"  arg:"a2a-public-url"        env:"A2A_PUBLIC_URL"        usage:"externally reachable A2A endpoint the card advertises"`
	A2AAgentName    string `required:"false" arg:"a2a-agent-name"        env:"A2A_AGENT_NAME"        usage:"agent name the Agent Card advertises"                                                                 default:"a2a-worker-controller"`
	A2AAuthToken    string `required:"true"  arg:"a2a-auth-token"        env:"A2A_AUTH_TOKEN"        usage:"bearer token every gated request must present"                                       display:"length"`
	Namespace       string `required:"false" arg:"namespace"             env:"NAMESPACE"             usage:"k8s backend: namespace every worker is created in"`
	WorkerTemplate  string `required:"false" arg:"worker-template"       env:"WORKER_TEMPLATE"       usage:"k8s backend: Config every worker is cloned from"                                                      default:"claude-interactive"`
	IngressDomain   string `required:"false" arg:"ingress-domain"        env:"INGRESS_DOMAIN"        usage:"k8s backend: parent domain of every worker host"`
	TLSSecretName   string `required:"false" arg:"tls-secret-name"       env:"TLS_SECRET_NAME"       usage:"k8s backend: wildcard certificate Secret for the ingress domain"`
	WorkerPriority  string `required:"false" arg:"worker-priority-class" env:"WORKER_PRIORITY_CLASS" usage:"k8s backend: PriorityClass every worker pod runs under (empty keeps the template's)"`
	EgressAllow     string `required:"false" arg:"egress-allow"          env:"EGRESS_ALLOW"          usage:"k8s backend: namespace:port list every worker may reach besides DNS"`
	BuildGitVersion string `required:"false" arg:"build-git-version"     env:"BUILD_GIT_VERSION"     usage:"Build Git version"                                                                                    default:"dev"`
	BuildGitCommit  string `required:"false" arg:"build-git-commit"      env:"BUILD_GIT_COMMIT"      usage:"Build Git commit hash"                                                                                default:"none"`
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

		// Wiring a backend is a change to this file by design — the provider is the single
		// place that maps a backend name to its implementation.
		//
		// The image is the one setting the backend cannot default. An untagged reference
		// resolves to :latest, and no :latest is published for the worker image, so a
		// default here would fail at create time with docker's own resolution error —
		// after the controller had already accepted the request — instead of at startup.
		// That is why it is required rather than defaulted, not for tidiness.
		dockerConfig := pkg.DefaultDockerConfig()
		dockerConfig.Image = a.WorkerImage
		kubernetesBackend, err := a.kubernetesBackend(ctx, backend)
		if err != nil {
			return err
		}
		provider := pkg.NewBackendProvider(
			pkg.NewDockerBackend(
				pkg.NewDockerCLIClient(),
				pkg.NewTCPPortProber(),
				dockerConfig,
			),
			kubernetesBackend,
		)
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

		// The gate wraps the whole router, so /a2a and every route registered after this
		// point are covered by construction rather than by remembering to add it. It is
		// built from a required token, so a controller that cannot authenticate refuses to
		// start rather than serving a surface that creates and destroys containers.
		slog.Info("starting http server", "listen", a.Listen)
		return libhttp.NewServer(
			a.Listen,
			pkg.NewAuthToken(a.A2AAuthToken).Wrap(router),
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

// kubernetesBackend builds the k8s backend when it is the one selected, and nil otherwise.
//
// It is built only when selected because its client loads the pod's in-cluster service
// account, which does not exist on a laptop running the docker backend — building it
// unconditionally would make the docker controller refuse to start. A nil backend is refused
// by the provider on use, so an unselected k8s backend can never be reached half-built.
//
// Its settings are checked here rather than marked required, because they are required only
// for this backend: a controller that would write workers into an unnamed namespace, or
// advertise a host nothing serves, refuses to start instead of failing on the first create.
func (a *application) kubernetesBackend(
	ctx context.Context,
	backend pkg.Backend,
) (pkg.WorkerBackend, error) {
	if backend != pkg.BackendKubernetes {
		return nil, nil
	}
	if a.Namespace == "" || a.IngressDomain == "" || a.TLSSecretName == "" {
		return nil, errors.Errorf(
			ctx,
			"backend %q requires NAMESPACE, INGRESS_DOMAIN and TLS_SECRET_NAME",
			backend,
		)
	}
	egressAllow, err := pkg.ParseEgressDestinations(ctx, a.EgressAllow)
	if err != nil {
		return nil, err
	}
	client, err := pkg.NewInClusterKubernetesClient(ctx, a.Namespace)
	if err != nil {
		return nil, err
	}
	config := pkg.DefaultKubernetesConfig()
	config.Namespace = a.Namespace
	config.TemplateConfig = a.WorkerTemplate
	config.IngressDomain = a.IngressDomain
	config.TLSSecretName = a.TLSSecretName
	config.EgressAllow = egressAllow
	config.WorkerPriorityClass = a.WorkerPriority
	return pkg.NewKubernetesBackend(
		client,
		pkg.NewWorkerResourceBuilder(config),
		config.TemplateConfig,
	), nil
}
