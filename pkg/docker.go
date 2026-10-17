// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkg

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/bborbe/errors"
)

// The docker label scheme. Every worker container carries all three labels; the controller
// label is what list and stop filter on, so a container without it is never touched.
//
// The keys deliberately contain no token, auth or secret: a label is readable by anyone who
// can run `docker ps`, so no credential may ever travel in one.
const (
	// controllerLabel marks a container as one this controller owns. Its presence is the
	// whole ownership rule — every read filters on it and every mutation resolves through it.
	controllerLabel = "com.bborbe.a2a-worker-controller"
	// controllerLabelValue is the controller label's constant identity value. It is the same
	// on every worker: the key's presence is the identity, the value only has to be stable.
	controllerLabelValue = "true"
	// workerIDLabel carries the worker's id, read back on list so a worker survives a
	// controller restart rather than being rebuilt from controller memory.
	workerIDLabel = "com.bborbe.a2a-worker-controller.worker-id"
	// taskLabel carries the vault task the worker serves. Create dedups on it, so a repeated
	// create is refused rather than starting a second worker.
	taskLabel = "com.bborbe.a2a-worker-controller.task"
)

// The worker container's fixed settings. They mirror the local-docker-run recipe the agent
// image documents, which is the command create automates.
const (
	// DefaultDockerImage is the image every worker runs. ⚠️ It is untagged, so docker
	// resolves it to :latest — and no :latest is published for this image, upstream or
	// locally, so a create that used this value verbatim would fail with docker's own
	// resolution error rather than anything naming the real cause. The binary therefore
	// requires the image to be configured and overrides this field; it survives as the
	// package's own placeholder, not as a value that resolves.
	DefaultDockerImage = "bborbe/agent-claude"
	// DefaultAgentNameBase prefixes the name each worker's Agent Card advertises; the worker
	// id is appended so two workers never advertise the same name.
	DefaultAgentNameBase = "claude-interactive"
	// DefaultAnthropicModel is the model the worker's turns run against.
	DefaultAnthropicModel = "MiniMax-M2.7-highspeed"
	// DefaultAllowedTools is the tool allow-list the worker's agent runs with.
	DefaultAllowedTools = "Read,Bash,Grep,Glob,Write,Edit,WebSearch,WebFetch"
	// DefaultPortStart and DefaultPortEnd bound the host-port search. 9090 is commonly taken
	// on a laptop and 9091/9092 are held by hand-started containers that are out of bounds,
	// so the search starts above them.
	DefaultPortStart = 9093
	DefaultPortEnd   = 9199
)

// The container's own listen port. The host port is published onto it, so the container
// always listens on the same port inside and the host port is the only thing that varies.
const containerPort = 9090

// The model router. Inside a container 127.0.0.1 is the container itself, so the router
// running on the host is reached through the docker host gateway.
const routerBaseURL = "http://host.docker.internal:8788"

// tokenEnvName is the container's bearer-token variable. The run command names it bare and
// the value travels in the child process's environment, so `ps` cannot read it.
const tokenEnvName = "INTERACTIVE_AUTH_TOKEN"

// The number of random bytes behind a worker id and behind a worker token.
const (
	workerIDBytes = 8
	tokenBytes    = 24
)

// DockerConfig holds the docker backend's settings. It carries no credential: the token a
// worker is created with is minted per worker and returned once.
type DockerConfig struct {
	// Image is the image every worker container runs.
	Image string
	// AgentNameBase prefixes the Agent Card name each worker advertises.
	AgentNameBase string
	// Model is the model the worker's turns run against.
	Model string
	// AllowedTools is the tool allow-list the worker's agent runs with.
	AllowedTools string
	// PortStart and PortEnd bound the host ports the backend will publish on.
	PortStart int
	PortEnd   int
	// MountAllowlist is the set of host paths a caller may ask this backend to expose. Its
	// zero value allows nothing, so a backend built without it refuses every mount — see
	// MountAllowlist for why the empty default is the safe one here.
	MountAllowlist MountAllowlist
	// TeamVault resolves a declared secret's key to its value. A nil resolver refuses any
	// create that declares a secret, rather than starting a worker whose secret is missing.
	TeamVault TeamVaultResolver
	// SecretDir is the host directory the resolved secret files are written into before being
	// bind-mounted read-only into the worker. It must be private to this controller: every file
	// in it holds a credential, and one controller's workers must never be able to read
	// another's.
	SecretDir string
}

// DefaultDockerConfig is the configuration the controller uses when none is supplied. Its
// values are the local-docker-run recipe's own.
func DefaultDockerConfig() DockerConfig {
	return DockerConfig{
		Image:         DefaultDockerImage,
		AgentNameBase: DefaultAgentNameBase,
		Model:         DefaultAnthropicModel,
		AllowedTools:  DefaultAllowedTools,
		PortStart:     DefaultPortStart,
		PortEnd:       DefaultPortEnd,
		// MountAllowlist is deliberately absent: its zero value refuses every mount, and the
		// binary is what supplies the operator's configured list.
		TeamVault: NewTeamVaultResolver(),
		SecretDir: DefaultSecretDir(),
	}
}

// DockerClient runs one docker CLI invocation. It is the backend's only door to the daemon
// and it is injected, so the backend reads no global and a test drives it with a fake that
// records the argv and the environment it was handed and answers with canned output — no
// daemon required.
type DockerClient interface {
	// Run runs docker with args and the extra environment, returning its standard output.
	// The extra environment is layered on the caller's own and is where a credential
	// travels, so the value never reaches argv.
	Run(ctx context.Context, args []string, extraEnv []string) (string, error)
}

// execDockerClient is the DockerClient that shells out to the docker CLI.
type execDockerClient struct{}

var _ DockerClient = (*execDockerClient)(nil)

// NewDockerCLIClient creates the DockerClient that runs the docker CLI.
func NewDockerCLIClient() DockerClient {
	return &execDockerClient{}
}

// Run runs the docker CLI. The child inherits this process's environment plus extraEnv, so a
// bare `-e NAME` in args resolves to the value this process already holds rather than to
// anything written on the command line.
func (c *execDockerClient) Run(
	ctx context.Context,
	args []string,
	extraEnv []string,
) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = append(os.Environ(), extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// args carries no credential by construction: every secret variable is named bare
		// and its value travels in the environment, so this message is safe to emit.
		return "", errors.Wrapf(
			ctx,
			err,
			"docker %s failed: %s",
			strings.Join(args, " "),
			strings.TrimSpace(stderr.String()),
		)
	}
	return stdout.String(), nil
}

// PortProber reports whether a host port is free to bind. It is injected so the port search
// is testable without opening a socket.
type PortProber interface {
	// IsFree reports whether port can be bound on the host.
	IsFree(ctx context.Context, port int) bool
}

// tcpPortProber is the PortProber that bind-checks a port on the host.
type tcpPortProber struct{}

var _ PortProber = (*tcpPortProber)(nil)

// NewTCPPortProber creates the PortProber that bind-checks a port on the host.
func NewTCPPortProber() PortProber {
	return &tcpPortProber{}
}

// IsFree reports whether port can be bound. A successful bind is released immediately: the
// port is not reserved, only proven free at the moment it was checked.
func (p *tcpPortProber) IsFree(_ context.Context, port int) bool {
	listener, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		return false
	}
	_ = listener.Close()
	return true
}

// dockerBackend runs each worker as a container on the local host. It is the only production
// implementation of WorkerBackend that names docker; the schema, the executor and the
// selection wiring stay backend-independent.
//
// It holds no worker state between calls. Every list reads the daemon and every create dedups
// against it — refusing a task that already has a worker rather than answering with one no
// caller could authenticate to — so a controller restart loses nothing and a second controller
// sharing the daemon sees the same workers.
type dockerBackend struct {
	client DockerClient
	prober PortProber
	config DockerConfig
	// mutex serializes Create, because both of its read-then-act pairs race otherwise: the
	// dedup read and the port search each happen before the docker run that acts on them,
	// so two overlapping creates can start two containers for one task, or pick one port
	// twice. It closes those within this process only — two controllers sharing a daemon
	// still race, which is why the port search reads the daemon rather than trusting a
	// single reading.
	mutex sync.Mutex
}

var _ WorkerBackend = (*dockerBackend)(nil)

// NewDockerBackend creates the WorkerBackend that runs each worker as a docker container.
//
// client is the backend's only door to the daemon and prober decides which host port is
// free; both are injected, so the backend reads no global and a test drives it without a
// daemon. config carries the image, the Agent Card name prefix, the model and the host-port
// range.
func NewDockerBackend(client DockerClient, prober PortProber, config DockerConfig) WorkerBackend {
	return &dockerBackend{client: client, prober: prober, config: config}
}

// Create starts one worker container for the requested task and returns it with its own
// freshly minted bearer token.
//
// ⚠️ A TASK THAT ALREADY HAS A CONTAINER IS REFUSED, NOT ANSWERED. Only one worker is ever
// started per task, but that worker's token cannot be returned — the controller kept no copy
// of it — so a success reply would name a worker no caller could authenticate to: it reads as
// a working worker while being unreachable, and the caller has nothing to reap it with. The
// refusal carries the existing worker's id instead, which is what makes the caller's own
// recovery — stop that worker, then create another — possible. That recovery stays on the
// caller by contract; this method only has to make it reachable.
func (b *dockerBackend) Create(ctx context.Context, req CreateRequest) (*CreateResponse, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	// Refused before any work, so a request carrying a reserved name fails fast rather than
	// after a port and a token have been minted for a worker that will not start.
	if err := validateExtraEnv(ctx, req.Env); err != nil {
		return nil, err
	}

	// Refused here for the same reason as the env check above, and with more at stake: a mount
	// outside the allowlist is an arbitrary host path on a public surface, and a secret this
	// controller cannot resolve would leave a worker that starts and fails at its first
	// authenticated call. Both are settled before a port, a token or a container exists.
	if err := ValidateMounts(ctx, b.config.MountAllowlist, req.Mounts); err != nil {
		return nil, err
	}
	if err := ValidateSecrets(ctx, req.Secrets); err != nil {
		return nil, err
	}

	containers, err := b.listContainers(ctx)
	if err != nil {
		return nil, err
	}
	if existing := findContainerByTask(containers, req.Task); existing != nil {
		// ⚠️ THE WORKER ID IS THE POINT OF THIS ERROR, NOT DECORATION. It is the only handle a
		// caller gets on a container it cannot authenticate to, and `stop` resolves ownership
		// from that id alone — so naming it is what turns an unreachable worker into a reaped
		// one. An error without it would leave the caller exactly as stuck as the empty-token
		// reply this replaces, only with a worse failure mode: a refusal that says nothing
		// actionable.
		return nil, errors.Errorf(
			ctx,
			"task %q already has worker %q, whose token this controller did not keep; "+
				"stop that worker and create another",
			req.Task,
			existing.WorkerID,
		)
	}

	port, err := b.freePort(ctx)
	if err != nil {
		return nil, err
	}

	workerID, err := newWorkerID(ctx)
	if err != nil {
		return nil, err
	}

	token, err := newToken(ctx)
	if err != nil {
		return nil, err
	}

	resolved, err := resolveSecrets(ctx, b.config.TeamVault, req.Secrets)
	if err != nil {
		return nil, err
	}
	secretPaths, err := WriteSecretFiles(ctx, b.config.SecretDir, workerID, resolved)
	if err != nil {
		return nil, err
	}

	args, env := createRun(
		b.config, workerID, req.Task, port, token, req.Env, req.Mounts, secretPaths,
	)
	if _, err := b.client.Run(ctx, args, env); err != nil {
		// The container never started, so the files written above have no reader. Leaving them
		// would strand a credential on disk for a worker that does not exist — and nothing else
		// would ever collect it, because stop only ever runs for a worker that started.
		if cleanupErr := RemoveSecretFiles(ctx, b.config.SecretDir, workerID); cleanupErr != nil {
			return nil, errors.Wrap(ctx, cleanupErr, "clean up secret files after failed create")
		}
		return nil, errors.Wrap(ctx, err, "create worker container failed")
	}

	return &CreateResponse{
		Worker: Worker{
			WorkerID: workerID,
			Task:     req.Task,
			A2AURL:   a2aURLForPort(strconv.Itoa(port)),
			Backend:  BackendDocker,
			State:    WorkerStateRunning,
		},
		Token: token,
	}, nil
}

// List returns every worker container this controller owns, rebuilt from the daemon on every
// call. It reads no cached state, so a controller restart loses nothing, and it never
// returns a container that lacks the controller label.
func (b *dockerBackend) List(ctx context.Context) (*ListResponse, error) {
	containers, err := b.listContainers(ctx)
	if err != nil {
		return nil, err
	}
	workers := make([]Worker, 0, len(containers))
	for _, container := range containers {
		workers = append(workers, container.worker())
	}
	return &ListResponse{Workers: workers}, nil
}

// Stop stops and removes the worker container the request names, resolving ownership from
// the container's own labels. A worker the controller does not own — or one that does not
// exist — is refused with an error and left running, so a stop can never touch a container
// this controller did not create.
func (b *dockerBackend) Stop(ctx context.Context, req StopRequest) (*StopResponse, error) {
	containers, err := b.listContainers(ctx)
	if err != nil {
		return nil, err
	}
	container := findContainerByWorkerID(containers, req.WorkerID)
	if container == nil {
		return nil, errors.Errorf(ctx, "worker %q is not owned by this controller", req.WorkerID)
	}
	if _, err := b.client.Run(ctx, []string{"stop", container.Name}, nil); err != nil {
		return nil, errors.Wrap(ctx, err, "stop worker container failed")
	}
	if _, err := b.client.Run(ctx, []string{"rm", container.Name}, nil); err != nil {
		return nil, errors.Wrap(ctx, err, "remove worker container failed")
	}
	// The container is gone, so its secret files have no reader and are removed here — the one
	// place that knows a worker is finished. After the container rather than before: the mounts
	// are held for its lifetime, so a removal that raced the stop would leave a running worker
	// holding a path that no longer exists.
	if err := RemoveSecretFiles(ctx, b.config.SecretDir, container.WorkerID); err != nil {
		return nil, err
	}
	worker := container.worker()
	worker.State = WorkerStateStopped
	return &StopResponse{Worker: worker}, nil
}

// listContainers reads every running container that carries the controller label. The filter
// is the whole ownership rule: a container without the label is never returned, so no other
// call in this backend can reach one.
func (b *dockerBackend) listContainers(ctx context.Context) ([]dockerContainer, error) {
	stdout, err := b.client.Run(ctx, listArgs(), nil)
	if err != nil {
		return nil, errors.Wrap(ctx, err, "list worker containers failed")
	}
	return parseContainers(stdout), nil
}

// freePort returns the first host port in the configured range that neither a running
// container publishes nor the host already holds.
//
// Both checks are needed and neither is sufficient. The daemon knows its own published
// ports; the bind check knows about processes that are not containers.
func (b *dockerBackend) freePort(ctx context.Context) (int, error) {
	published, err := b.publishedPorts(ctx)
	if err != nil {
		return 0, err
	}
	for port := b.config.PortStart; port <= b.config.PortEnd; port++ {
		if published[port] {
			continue
		}
		if b.prober.IsFree(ctx, port) {
			return port, nil
		}
	}
	return 0, errors.Errorf(
		ctx,
		"no free host port in range %d-%d",
		b.config.PortStart,
		b.config.PortEnd,
	)
}

// publishedPorts returns every host port a running container publishes.
//
// This is read from the daemon rather than probed on a socket, and that is load-bearing
// rather than defensive. On macOS, Docker Desktop forwards a published port in userspace:
// nothing on the host holds it from the operating system's point of view, so a bind check
// reports a port a running container has already published as free. Trusting the bind check
// alone makes the search return its FIRST candidate on every call, so the first create takes
// the port and every later create collides with it — docker answers `Bind for 0.0.0.0:<port>
// failed: port is already allocated` after it has already created the container, leaving a
// never-started container behind that carries the task label the dedup reads.
func (b *dockerBackend) publishedPorts(ctx context.Context) (map[int]bool, error) {
	stdout, err := b.client.Run(
		ctx,
		[]string{"ps", "--no-trunc", "--format", "{{.Ports}}"},
		nil,
	)
	if err != nil {
		return nil, errors.Wrap(ctx, err, "list published ports failed")
	}
	return publishedHostPorts(stdout), nil
}

// publishedHostPorts parses `docker ps --format '{{.Ports}}'` output into the set of host
// ports it publishes. Each mapping reads `0.0.0.0:9093->9090/tcp`, so the host port is the
// number after the last colon on the left of the arrow. A container publishing nothing
// contributes an empty line, and a line without an arrow contributes nothing.
func publishedHostPorts(stdout string) map[int]bool {
	ports := map[int]bool{}
	for _, line := range strings.Split(stdout, "\n") {
		for _, mapping := range strings.Split(line, ",") {
			mapping = strings.TrimSpace(mapping)
			arrow := strings.Index(mapping, "->")
			if arrow < 0 {
				continue
			}
			host := mapping[:arrow]
			colon := strings.LastIndex(host, ":")
			if colon < 0 {
				continue
			}
			port, err := strconv.Atoi(host[colon+1:])
			if err != nil {
				continue
			}
			ports[port] = true
		}
	}
	return ports
}

// listArgs is the docker ps invocation every read uses. The filter is the controller label,
// so the daemon does the ownership filtering and the parser never sees a foreign container.
func listArgs() []string {
	return []string{
		"ps",
		"--filter", "label=" + controllerLabel,
		"--no-trunc",
		"--format", "{{.Names}}\t{{.Label \"" + workerIDLabel + "\"}}\t{{.Label \"" +
			taskLabel + "\"}}\t{{.Ports}}",
	}
}

// createRun builds the argv and the extra child environment for one worker's docker run.
//
// The token is a parameter and never an argument: the run command names INTERACTIVE_AUTH_TOKEN
// bare, and the value is returned in the environment slice instead. The credential therefore
// reaches the container through the client process's environment and can never be read back
// out of `ps`. The model router's key is named bare the same way and resolves from this
// process's own environment, so the controller never holds it either.
//
// Caller-supplied entries follow that same rule, deliberately: the controller cannot tell a
// credential from a plain setting, and create is a public surface, so every entry the caller
// sends is named bare in argv and carried by the child environment. Nothing a caller passes
// can be read back out of `ps`.
func createRun(
	config DockerConfig,
	workerID WorkerID,
	task TaskName,
	port int,
	token string,
	extraEnv map[string]string,
	mounts []Mount,
	secretMounts []SecretMount,
) ([]string, []string) {
	baseArgs := []string{
		"run", "-d",
		"--name", containerName(workerID),
		"--label", controllerLabel + "=" + controllerLabelValue,
		"--label", workerIDLabel + "=" + string(workerID),
		"--label", taskLabel + "=" + string(task),
		"-p", strconv.Itoa(port) + ":" + strconv.Itoa(containerPort),
		"-e", "AGENT_TYPE=service",
		"-e", "LISTEN=:" + strconv.Itoa(containerPort),
		"-e", "A2A_PUBLIC_URL=" + a2aURLForPort(strconv.Itoa(port)),
		"-e", "A2A_AGENT_NAME=" + agentNameFor(config, workerID),
		"-e", tokenEnvName,
		"-e", "ANTHROPIC_BASE_URL=" + routerBaseURL,
		"-e", "ANTHROPIC_AUTH_TOKEN",
		"-e", "ANTHROPIC_MODEL=" + config.Model,
		"-e", "ALLOWED_TOOLS=" + config.AllowedTools,
	}
	// Both slices are sized up front rather than grown: each extra entry contributes one bare
	// `-e NAME` pair to the argv, and the image closes it.
	args := make([]string, 0, len(baseArgs)+2*len(extraEnv)+2*len(mounts)+2*len(secretMounts)+1)
	args = append(args, baseArgs...)
	childEnv := make([]string, 0, 1+len(extraEnv))
	childEnv = append(childEnv, tokenEnvName+"="+token)
	// Sorted, because ranging a map in Go is deliberately randomised: an unsorted argv would
	// make two identical creates produce different command lines, which no test could assert.
	for _, key := range sortedKeys(extraEnv) {
		args = append(args, "-e", key)
		childEnv = append(childEnv, key+"="+extraEnv[key])
	}
	// Paths come last, immediately before the image, so the argv reads in the order the flags
	// above it are organised: settings, then what the worker can see, then what to run.
	//
	// ⚠️ A DECLARED MOUNT IS WRITABLE UNLESS THE CALLER SET read_only; A SECRET MOUNT NEVER IS.
	// The asymmetry is deliberate. A caller asking for a vault it will write to has to say so,
	// while a credential is something a worker reads — and a writable secret mount would let a
	// compromised worker rewrite the file the next worker for that task mounts.
	for _, mount := range mounts {
		spec := mount.HostPath + ":" + mount.ContainerPath
		if mount.ReadOnly {
			spec += ":ro"
		}
		args = append(args, "-v", spec)
	}
	for _, secret := range secretMounts {
		args = append(args, "-v", secret.HostPath+":"+SecretContainerDir+"/"+secret.Name+":ro")
	}
	args = append(args, config.Image)
	return args, childEnv
}

// sortedKeys returns a map's keys in lexical order, so an argv built from them is
// deterministic.
func sortedKeys(entries map[string]string) []string {
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// reservedEnvNames are the variables createRun sets itself. A caller-supplied entry naming one
// of these is refused rather than merged.
//
// The guard exists for one name above all: AGENT_TYPE selects the agent's shape, and a caller
// free to overwrite it could start a worker outside the service path, where no permission
// decider is wired and the worker's gate silently disappears. The rest are refused for the
// same reason at lower stakes — each is a setting the controller owns, and a request that
// could rewrite one would make the container's configuration depend on its caller.
var reservedEnvNames = map[string]struct{}{
	"AGENT_TYPE":           {},
	"LISTEN":               {},
	"A2A_PUBLIC_URL":       {},
	"A2A_AGENT_NAME":       {},
	tokenEnvName:           {},
	"ANTHROPIC_BASE_URL":   {},
	"ANTHROPIC_AUTH_TOKEN": {},
	"ANTHROPIC_MODEL":      {},
	"ALLOWED_TOOLS":        {},
}

// validateExtraEnv refuses a caller-supplied environment that would overwrite a setting the
// controller owns. It names the offending key so the caller can drop it rather than guess.
func validateExtraEnv(ctx context.Context, extraEnv map[string]string) error {
	for _, key := range sortedKeys(extraEnv) {
		if _, reserved := reservedEnvNames[key]; reserved {
			return errors.Errorf(
				ctx,
				"env %q is set by the controller and cannot be supplied by a create request",
				key,
			)
		}
	}
	return nil
}

// dockerContainer is one worker container as the daemon reports it. It holds no credential:
// a token is never written to a label, so nothing here can carry one.
type dockerContainer struct {
	Name     string
	WorkerID WorkerID
	Task     TaskName
	HostPort string
}

// worker renders the container as the control API's Worker shape. A container the daemon
// reports is running by definition — list reads `docker ps` without --all — so its state is
// running.
func (c dockerContainer) worker() Worker {
	return Worker{
		WorkerID: c.WorkerID,
		Task:     c.Task,
		A2AURL:   a2aURLForPort(c.HostPort),
		Backend:  BackendDocker,
		State:    WorkerStateRunning,
	}
}

// parseContainers reads one docker ps line per container. A line that does not carry both
// the worker id and the task label is skipped: the filter should have excluded it, but a
// partially labelled container must not become a half-built worker.
func parseContainers(stdout string) []dockerContainer {
	containers := make([]dockerContainer, 0)
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(fields) != 4 {
			continue
		}
		name := strings.TrimSpace(fields[0])
		workerID := strings.TrimSpace(fields[1])
		task := strings.TrimSpace(fields[2])
		if name == "" || workerID == "" || task == "" {
			continue
		}
		containers = append(containers, dockerContainer{
			Name:     name,
			WorkerID: WorkerID(workerID),
			Task:     TaskName(task),
			HostPort: hostPortFromPorts(fields[3]),
		})
	}
	return containers
}

// hostPortFromPorts reads the host port a container publishes onto its own port out of a
// docker ps Ports field: "0.0.0.0:9093->9090/tcp, :::9093->9090/tcp" yields "9093". It
// returns an empty string when no mapping onto the container port is present.
func hostPortFromPorts(ports string) string {
	suffix := "->" + strconv.Itoa(containerPort) + "/tcp"
	for _, mapping := range strings.Split(ports, ",") {
		mapping = strings.TrimSpace(mapping)
		if !strings.HasSuffix(mapping, suffix) {
			continue
		}
		host := strings.TrimSuffix(mapping, suffix)
		if index := strings.LastIndex(host, ":"); index >= 0 {
			return host[index+1:]
		}
	}
	return ""
}

// findContainerByTask returns the container serving task, or nil when none does. It is what
// makes a repeated create a no-op rather than a second container.
func findContainerByTask(containers []dockerContainer, task TaskName) *dockerContainer {
	for index := range containers {
		if containers[index].Task == task {
			return &containers[index]
		}
	}
	return nil
}

// findContainerByWorkerID returns the container carrying workerID, or nil when none does.
// Ownership is resolved from the label alone, so a stop can only ever name a container this
// controller created.
func findContainerByWorkerID(
	containers []dockerContainer,
	workerID WorkerID,
) *dockerContainer {
	for index := range containers {
		if containers[index].WorkerID == workerID {
			return &containers[index]
		}
	}
	return nil
}

// a2aURLForPort is the address a worker advertises, built from the host port it is published
// on. A2A_PUBLIC_URL is configuration and is never derived from the container's listen
// address, so the run command and this function must agree or every client posts to a dead
// endpoint.
func a2aURLForPort(port string) string {
	if port == "" {
		return ""
	}
	return "http://localhost:" + port + "/a2a"
}

// containerName is a worker's container name. It is derived from the worker id, which is
// itself read back from the container's label, so the name survives a controller restart.
func containerName(workerID WorkerID) string {
	return "a2a-worker-" + string(workerID)
}

// agentNameFor is the name a worker's Agent Card advertises. The worker id is appended so
// two workers never advertise the same name.
func agentNameFor(config DockerConfig, workerID WorkerID) string {
	return config.AgentNameBase + "-" + string(workerID)
}

// newWorkerID mints a worker id. It is random rather than derived from the task, so two
// workers never collide on a name, and it is written to the container's label so list and
// stop can read it back after a restart.
func newWorkerID(ctx context.Context) (WorkerID, error) {
	secret, err := newSecret(ctx, workerIDBytes, "worker id")
	if err != nil {
		return "", err
	}
	return WorkerID(secret), nil
}

// newToken mints one worker's bearer token. The value exists only in this call's return and
// in the container's environment: it is never written to a label, a log or a file, and the
// controller keeps no copy of it.
func newToken(ctx context.Context) (string, error) {
	return newSecret(ctx, tokenBytes, "worker token")
}

// newSecret returns n cryptographically random bytes as a lowercase hex string.
func newSecret(ctx context.Context, n int, what string) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", errors.Wrapf(ctx, err, "mint %s failed", what)
	}
	return hex.EncodeToString(buf), nil
}
