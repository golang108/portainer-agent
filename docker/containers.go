package docker

import (
	"context"
	"io"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
)

const largeClientTimeout = 1 * time.Hour

func ImagePull(refStr string, options types.ImagePullOptions) (io.ReadCloser, error) {
	var err error
	var reader io.ReadCloser

	err = withCli(func(cli *client.Client) error {
		cli.HTTPClient().Timeout = largeClientTimeout

		reader, err = cli.ImagePull(context.Background(), refStr, options)

		return err
	})

	return reader, err
}

func ContainerCreate(
	config *container.Config,
	hostConfig *container.HostConfig,
	networkingConfig *network.NetworkingConfig,
	platform *specs.Platform,
	containerName string,
) (container.CreateResponse, error) {
	var err error
	var createResponse container.CreateResponse

	err = withCli(func(cli *client.Client) error {
		cli.HTTPClient().Timeout = largeClientTimeout

		createResponse, err = cli.ContainerCreate(context.Background(), config, hostConfig, networkingConfig, platform, containerName)

		return err
	})

	return createResponse, err
}

func ContainerStart(name string, opts container.StartOptions) error {
	return withCli(func(cli *client.Client) error {
		return cli.ContainerStart(context.Background(), name, opts)
	})
}

func ContainerRestart(name string) error {
	return withCli(func(cli *client.Client) error {
		return cli.ContainerRestart(context.Background(), name, container.StopOptions{})
	})
}

func ContainerStop(name string) error {
	return withCli(func(cli *client.Client) error {
		return cli.ContainerStop(context.Background(), name, container.StopOptions{})
	})
}

func ContainerKill(name string) error {
	return withCli(func(cli *client.Client) error {
		return cli.ContainerKill(context.Background(), name, "KILL")
	})
}

func ContainerDelete(name string, opts container.RemoveOptions) error {
	return withCli(func(cli *client.Client) error {
		return cli.ContainerRemove(context.Background(), name, opts)
	})
}

func ContainerWait(name string, condition container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	var statusCh <-chan container.WaitResponse
	var errCh <-chan error

	if err := withCli(func(cli *client.Client) error {
		cli.HTTPClient().Timeout = largeClientTimeout

		statusCh, errCh = cli.ContainerWait(context.Background(), name, condition)

		return nil
	}); err != nil {
		ch := make(chan error)
		errCh = ch

		go func() { ch <- err }()
	}

	return statusCh, errCh
}
