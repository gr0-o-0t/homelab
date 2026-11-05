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
	"github.com/docker/docker/api/types/volume"
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
	SwarmInit(ctx context.Context, req swarm.InitRequest) (string, error)
	SwarmJoin(ctx context.Context, req swarm.JoinRequest) error
	SwarmInspect(ctx context.Context) (swarm.Swarm, error)
	Events(ctx context.Context, options events.ListOptions) (<-chan events.Message, <-chan error)
	ContainerList(ctx context.Context, options container.ListOptions) ([]container.Summary, error)
	ContainerExecCreate(ctx context.Context, container string, options container.ExecOptions) (container.ExecCreateResponse, error)
	ContainerExecAttach(ctx context.Context, execID string, options container.ExecAttachOptions) (types.HijackedResponse, error)
}
