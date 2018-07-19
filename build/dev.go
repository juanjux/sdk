package build

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"google.golang.org/grpc"
	"gopkg.in/bblfsh/sdk.v2/internal/docker"
	"gopkg.in/bblfsh/sdk.v2/protocol"
)

const (
	cliPort      = "9432"
	dockerSchema = "docker-daemon:"
)

type ServerInstance struct {
	cli     *docker.Client
	user    *grpc.ClientConn
	bblfshd *docker.Container
}

func (d *ServerInstance) installFromDocker(ctx context.Context, lang, id string) error {
	cmd := []string{"bblfshctl", "driver", "install", lang, dockerSchema + id}
	printCommand("docker", append([]string{"exec", id}, cmd...)...)
	e, err := d.cli.CreateExec(docker.CreateExecOptions{
		Context:      ctx,
		Container:    d.bblfshd.ID,
		AttachStdout: true, AttachStderr: true,
		Cmd: cmd,
	})
	if err != nil {
		return err
	}
	buf := bytes.NewBuffer(nil)
	err = d.cli.StartExec(e.ID, docker.StartExecOptions{
		Context:      ctx,
		OutputStream: buf, ErrorStream: buf,
	})
	if err != nil {
		return err
	} else if str := buf.String(); strings.Contains(strings.ToLower(str), "error") {
		return errors.New(strings.TrimSpace(str))
	}
	return nil
}
func (d *ServerInstance) ClientV1(ctx context.Context) (protocol.ProtocolServiceClient, error) {
	if d.user == nil {
		addr := d.bblfshd.NetworkSettings.IPAddress
		conn, err := grpc.DialContext(ctx, addr+":"+cliPort, grpc.WithInsecure(), grpc.WithBlock())
		if err != nil {
			return nil, err
		}
		d.user = conn
	}
	return protocol.NewProtocolServiceClient(d.user), nil
}
func (s *ServerInstance) DumpLogs(w io.Writer) error {
	return getLogs(s.cli, s.bblfshd.ID, w)
}
func (d *ServerInstance) Close() error {
	if d.user != nil {
		_ = d.user.Close()
	}
	return d.cli.RemoveContainer(docker.RemoveContainerOptions{
		ID: d.bblfshd.ID, Force: true,
	})
}

// RunWithDriver starts a bblfshd server and installs a specified driver to it.
func RunWithDriver(lang, id string) (*ServerInstance, error) {
	cli, err := docker.Dial()
	if err != nil {
		return nil, err
	}
	const (
		bblfshd = "bblfsh/bblfshd"
		// needed to install driver from Docker instance
		sock = docker.Socket + ":" + docker.Socket
	)

	printCommand("docker", "run", "--rm", "--privileged", "-v", sock, bblfshd)
	c, err := docker.Run(cli, docker.CreateContainerOptions{
		Config: &docker.Config{
			Image: bblfshd,
		},
		HostConfig: &docker.HostConfig{
			AutoRemove: true,
			Privileged: true,
			Binds:      []string{sock},
		},
	})
	if err != nil {
		return nil, err
	}
	s := &ServerInstance{cli: cli, bblfshd: c}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute*3)
	defer cancel()
	if err := s.installFromDocker(ctx, lang, id); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func getLogs(cli *docker.Client, id string, w io.Writer) error {
	return cli.AttachToContainer(docker.AttachToContainerOptions{
		Container: id, OutputStream: w, ErrorStream: w,
		Logs: true, Stdout: true, Stderr: true,
	})
}