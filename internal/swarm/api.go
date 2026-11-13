package swarm

import (
	"context"
	"io"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/system"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// API is the slice of the Docker Engine SDK that homelab uses to drive Swarm.
// Signatures match github.com/docker/docker v28, so *client.Client satisfies
// it; tests use swarmtest.Fake instead of a daemon.
type API interface {
	ServiceList(ctx context.Context, options swarm.ServiceListOptions) ([]swarm.Service, error)
	ServiceInspectWithRaw(ctx context.Context, serviceID string, options swarm.ServiceInspectOptions) (swarm.Service, []byte, error)
	ServiceCreate(ctx context.Context, service swarm.ServiceSpec, options swarm.ServiceCreateOptions) (swarm.ServiceCreateResponse, error)
	ServiceUpdate(ctx context.Context, serviceID string, version swarm.Version, service swarm.ServiceSpec, options swarm.ServiceUpdateOptions) (swarm.ServiceUpdateResponse, error)
	ServiceRemove(ctx context.Context, serviceID string) error
	TaskList(ctx context.Context, options swarm.TaskListOptions) ([]swarm.Task, error)
	NodeList(ctx context.Context, options swarm.NodeListOptions) ([]swarm.Node, error)
	NodeUpdate(ctx context.Context, nodeID string, version swarm.Version, node swarm.NodeSpec) error
	NetworkList(ctx context.Context, options network.ListOptions) ([]network.Summary, error)
	NetworkCreate(ctx context.Context, name string, options network.CreateOptions) (network.CreateResponse, error)
	NetworkRemove(ctx context.Context, networkID string) error
	// NetworkInspect lists a network's attached containers, which
	// NetworkList leaves out (init --import checks the Compose-era bridge).
	NetworkInspect(ctx context.Context, networkID string, options network.InspectOptions) (network.Inspect, error)
	SecretList(ctx context.Context, options swarm.SecretListOptions) ([]swarm.Secret, error)
	SecretCreate(ctx context.Context, secret swarm.SecretSpec) (swarm.SecretCreateResponse, error)
	SecretRemove(ctx context.Context, id string) error
	ConfigList(ctx context.Context, options swarm.ConfigListOptions) ([]swarm.Config, error)
	ConfigCreate(ctx context.Context, config swarm.ConfigSpec) (swarm.ConfigCreateResponse, error)
	ConfigRemove(ctx context.Context, id string) error
	VolumeList(ctx context.Context, options volume.ListOptions) (volume.ListResponse, error)
	// ImageInspectWithRaw is deprecated in v28 in favour of ImageInspect but
	// still part of client.APIClient.
	ImageInspectWithRaw(ctx context.Context, image string) (image.InspectResponse, []byte, error)
	ImagePull(ctx context.Context, ref string, options image.PullOptions) (io.ReadCloser, error)
	// ImageLoad reads a `docker save` tar into the engine (a custom
	// service's locally built image, shipped to the leader).
	ImageLoad(ctx context.Context, input io.Reader, opts ...client.ImageLoadOption) (image.LoadResponse, error)
	SwarmInit(ctx context.Context, req swarm.InitRequest) (string, error)
	SwarmJoin(ctx context.Context, req swarm.JoinRequest) error
	SwarmInspect(ctx context.Context) (swarm.Swarm, error)
	SwarmUpdate(ctx context.Context, version swarm.Version, spec swarm.Spec, flags swarm.UpdateFlags) error
	SwarmLeave(ctx context.Context, force bool) error
	Events(ctx context.Context, options events.ListOptions) (<-chan events.Message, <-chan error)
	ContainerList(ctx context.Context, options container.ListOptions) ([]container.Summary, error)
	ContainerExecCreate(ctx context.Context, container string, options container.ExecOptions) (container.ExecCreateResponse, error)
	ContainerExecAttach(ctx context.Context, execID string, options container.ExecAttachOptions) (types.HijackedResponse, error)
	ContainerExecInspect(ctx context.Context, execID string) (container.ExecInspect, error)
	// The agent container itself (init/join) is a plain container, not a
	// Swarm service: it must run before the node is in a swarm.
	ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig,
		networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, containerName string) (container.CreateResponse, error)
	ContainerStart(ctx context.Context, container string, options container.StartOptions) error
	ContainerRestart(ctx context.Context, container string, options container.StopOptions) error
	// ContainerInspect and ContainerStop serve `homelab upgrade`: the agent
	// container is recreated from its own inspected spec on a new image.
	ContainerInspect(ctx context.Context, container string) (container.InspectResponse, error)
	ContainerStop(ctx context.Context, container string, options container.StopOptions) error
	// ContainerRename keeps the old agent aside, intact, until its
	// replacement has proved itself (and puts it back otherwise).
	ContainerRename(ctx context.Context, container, newContainerName string) error
	// CopyToContainer extracts a tar stream at dstPath in a container
	// (init --import's state tree; never secrets).
	CopyToContainer(ctx context.Context, containerID, dstPath string, content io.Reader, options container.CopyToContainerOptions) error
	// ContainerRemove and VolumeRemove undo a failed init --import: the
	// agent container and its own state volume, never another volume.
	ContainerRemove(ctx context.Context, containerID string, options container.RemoveOptions) error
	VolumeRemove(ctx context.Context, volumeID string, force bool) error
	// Info reports this engine's swarm membership (node ID, manager or not).
	Info(ctx context.Context) (system.Info, error)
}
