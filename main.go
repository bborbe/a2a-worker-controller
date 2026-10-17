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
	SentryDSN         string `required:"true"  arg:"sentry-dsn"            env:"SENTRY_DSN"            usage:"SentryDSN"                                                                                              display:"length"`
	SentryProxy       string `required:"false" arg:"sentry-proxy"          env:"SENTRY_PROXY"          usage:"Sentry Proxy"`
	Listen            string `required:"true"  arg:"listen"                env:"LISTEN"                usage:"address to listen to"`
	Backend           string `required:"false" arg:"backend"               env:"BACKEND"               usage:"backend serving workers"                                                                                                 default:"docker"`
	WorkerImage       string `required:"true"  arg:"worker-image"          env:"WORKER_IMAGE"          usage:"image every created worker container runs"`
	A2APublicURL      string `required:"true"  arg:"a2a-public-url"        env:"A2A_PUBLIC_URL"        usage:"externally reachable A2A endpoint the card advertises"`
	A2AAgentName      string `required:"false" arg:"a2a-agent-name"        env:"A2A_AGENT_NAME"        usage:"agent name the Agent Card advertises"                                                                                    default:"a2a-worker-controller"`
	A2AAuthToken      string `required:"true"  arg:"a2a-auth-token"        env:"A2A_AUTH_TOKEN"        usage:"bearer token every gated request must present"                                                          display:"length"`
	Namespace         string `required:"false" arg:"namespace"             env:"NAMESPACE"             usage:"k8s backend: namespace every worker is created in"`
	WorkerTemplate    string `required:"false" arg:"worker-template"       env:"WORKER_TEMPLATE"       usage:"k8s backend: Config every worker is cloned from"                                                                         default:"claude-interactive"`
	IngressDomain     string `required:"false" arg:"ingress-domain"        env:"INGRESS_DOMAIN"        usage:"k8s backend: parent domain of every worker host"`
	TLSSecretName     string `required:"false" arg:"tls-secret-name"       env:"TLS_SECRET_NAME"       usage:"k8s backend: wildcard certificate Secret for the ingress domain"`
	WorkerPriority    string `required:"false" arg:"worker-priority-class" env:"WORKER_PRIORITY_CLASS" usage:"k8s backend: PriorityClass every worker pod runs under (empty keeps the template's)"`
	EgressAllow       string `required:"false" arg:"egress-allow"          env:"EGRESS_ALLOW"          usage:"k8s backend: namespace[/key=value]:port list every worker may reach besides DNS"`
	MountAllow        string `required:"false" arg:"mount-allow"           env:"MOUNT_ALLOW"           usage:"docker backend: comma-separated absolute host paths a caller may declare as mounts (empty allows none)"`
	LocalVault        string `required:"false" arg:"local-vault"           env:"LOCAL_VAULT"           usage:"local backend: vault root the workers' task files live in"`
	LocalClaudeScript string `required:"false" arg:"local-claude-script"   env:"LOCAL_CLAUDE_SCRIPT"   usage:"local backend: vault claude_script launcher every worker tab runs"`
	LocalWindowID     string `required:"false" arg:"local-window-id"       env:"LOCAL_WINDOW_ID"       usage:"local backend: WezTerm window id a worker tab opens in when its role does not resolve one"`
	LocalRoleMap      string `required:"false" arg:"local-role-map"        env:"LOCAL_ROLE_MAP"        usage:"local backend: WezTerm role map a task's role is resolved against"`
	LocalWezterm      string `required:"false" arg:"local-wezterm"         env:"LOCAL_WEZTERM"         usage:"local backend: absolute path to the wezterm binary (defaults to the app bundle)"`
	LocalStateFile    string `required:"false" arg:"local-state-file"      env:"LOCAL_STATE_FILE"      usage:"local backend: JSON file the worker ownership store lives in (defaults under ~/.local/state)"`
	BuildGitVersion   string `required:"false" arg:"build-git-version"     env:"BUILD_GIT_VERSION"     usage:"Build Git version"                                                                                                       default:"dev"`
	BuildGitCommit    string `required:"false" arg:"build-git-commit"      env:"BUILD_GIT_COMMIT"      usage:"Build Git commit hash"                                                                                                   default:"none"`
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
		// Parsed at startup rather than at the first create that needs it: an empty MOUNT_ALLOW
		// yields an empty allowlist, which refuses every declared mount, and a malformed entry
		// stops the controller while the operator is still looking at the config rather than at
		// a worker that failed to reach its vault hours later.
		mountAllowlist, err := pkg.ParseMountAllowlist(ctx, a.MountAllow)
		if err != nil {
			return err
		}
		dockerConfig.MountAllowlist = mountAllowlist
		kubernetesBackend, err := a.kubernetesBackend(ctx, backend)
		if err != nil {
			return err
		}
		localBackend, err := a.localBackend(ctx, backend)
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
			localBackend,
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
	case pkg.BackendDocker, pkg.BackendKubernetes, pkg.BackendLocal:
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
		config.TeamVault,
	), nil
}

// localBackend builds the local backend when it is the one selected, and nil otherwise.
//
// Built only when selected, like the k8s backend: every one of its settings exists for this
// backend alone, and a controller serving containers has no business holding a vault path or
// reaching WezTerm.
//
// Its two required settings are checked here rather than marked required, because they are
// required only for this backend. A controller that would open a tab with no launcher, or read
// a task file out of no vault, refuses to start instead of failing on the first create — after
// it had already accepted the request.
//
// The wezterm path and the store path are defaulted here from the package's own constants
// rather than in the flag tags, so the default has one home and the store's location is
// derived from the home directory rather than written into a struct tag.
func (a *application) localBackend(
	ctx context.Context,
	backend pkg.Backend,
) (pkg.WorkerBackend, error) {
	if backend != pkg.BackendLocal {
		return nil, nil
	}
	if a.LocalVault == "" || a.LocalClaudeScript == "" {
		return nil, errors.Errorf(
			ctx,
			"backend %q requires LOCAL_VAULT and LOCAL_CLAUDE_SCRIPT",
			backend,
		)
	}
	weztermBinary := a.LocalWezterm
	if weztermBinary == "" {
		weztermBinary = pkg.DefaultWeztermBinary
	}
	stateFile := a.LocalStateFile
	if stateFile == "" {
		defaultStateFile, err := pkg.DefaultLocalStateFile(ctx)
		if err != nil {
			return nil, err
		}
		stateFile = defaultStateFile
	}
	return pkg.NewLocalBackend(
		pkg.NewWeztermCLIClient(weztermBinary),
		pkg.NewJSONWorkerStore(stateFile),
		pkg.LocalConfig{
			VaultDir:     a.LocalVault,
			ClaudeScript: a.LocalClaudeScript,
			WindowID:     a.LocalWindowID,
			RoleMapPath:  a.LocalRoleMap,
		},
	), nil
}
