package docker

import (
	"context"
	"fmt"
	"github.com/RA341/dockman/internal/ssh"
	"github.com/RA341/dockman/pkg"
	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/flags"
	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/compose/v2/pkg/compose"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/rs/zerolog/log"
	"io"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
)

// reference: https://github.com/portainer/portainer/blob/develop/pkg/libstack/compose/composeplugin.go

type ComposeService struct {
	ComposeRoot string
	Client      *ContainerService
}

func newComposeService(composeRoot string, client *ContainerService) *ComposeService {
	return &ComposeService{
		ComposeRoot: composeRoot,
		Client:      client,
	}
}

func (s *ComposeService) Up(ctx context.Context, project *types.Project, composeClient api.Service, services ...string) error {
	if err := s.syncProjectFilesToHost(project); err != nil {
		return err
	}

	upOpts := api.UpOptions{
		Create: api.CreateOptions{
			Build:                &api.BuildOptions{Services: services},
			Services:             services,
			RemoveOrphans:        false,
			Recreate:             api.RecreateForce, // Force recreation of the specified services
			RecreateDependencies: api.RecreateNever, // Do not recreate dependencies
			Inherit:              true,
			AssumeYes:            true,
		},
		Start: api.StartOptions{
			//OnExit:   api.CascadeStop,
			Services: services,
		},
	}

	if err := composeClient.Up(ctx, project, upOpts); err != nil {
		return fmt.Errorf("compose up operation failed: %w", err)
	}

	return nil
}

func (s *ComposeService) Down(ctx context.Context, project *types.Project, composeClient api.Service, services ...string) error {
	downOpts := api.DownOptions{
		Services: services,
	}
	if err := composeClient.Down(ctx, project.Name, downOpts); err != nil {
		return fmt.Errorf("compose down operation failed: %w", err)
	}
	return nil
}

func (s *ComposeService) Stop(ctx context.Context, project *types.Project, composeClient api.Service, services ...string) error {
	stopOpts := api.StopOptions{
		Services: services,
	}
	if err := composeClient.Stop(ctx, project.Name, stopOpts); err != nil {
		return fmt.Errorf("compose stop operation failed: %w", err)
	}
	return nil
}

func (s *ComposeService) Restart(ctx context.Context, project *types.Project, composeClient api.Service, services ...string) error {
	// A restart might involve changes to the compose file, so we sync first.
	if err := s.syncProjectFilesToHost(project); err != nil {
		return err
	}

	restartOpts := api.RestartOptions{
		Services: services,
	}
	if err := composeClient.Restart(ctx, project.Name, restartOpts); err != nil {
		return fmt.Errorf("compose restart operation failed: %w", err)
	}
	return nil
}

func (s *ComposeService) Pull(ctx context.Context, project *types.Project, composeClient api.Service) error {
	pullOpts := api.PullOptions{}
	if err := composeClient.Pull(ctx, project, pullOpts); err != nil {
		return fmt.Errorf("compose pull operation failed: %w", err)
	}
	return nil
}

func (s *ComposeService) Update(ctx context.Context, project *types.Project, composeClient api.Service, services ...string) error {
	beforeImages, err := s.getProjectImageDigests(ctx, project)
	if err != nil {
		return fmt.Errorf("failed to get image info before pull: %w", err)
	}

	if err = s.Pull(ctx, project, composeClient); err != nil {
		return err
	}

	afterImages, err := s.getProjectImageDigests(ctx, project)
	if err != nil {
		return fmt.Errorf("failed to get image info after pull: %w", err)
	}

	// Compare digests to see if anything changed.
	if reflect.DeepEqual(beforeImages, afterImages) {
		log.Info().Str("stack", project.Name).Msg("No new images were downloaded, stack is up to date")
		return nil
	}

	log.Info().Str("stack", project.Name).Msg("New images were downloaded, updating stack...")
	// If images changed, run Up to recreate the containers with the new images.
	if err = s.Up(ctx, project, composeClient, services...); err != nil {
		return err
	}

	return nil
}

// ListStack The `all` parameter controls whether to show stopped containers.
func (s *ComposeService) ListStack(ctx context.Context, project *types.Project, all bool) ([]container.Summary, error) {
	containerFilters := filters.NewArgs()
	projectLabel := fmt.Sprintf("%s=%s", api.ProjectLabel, project.Name)
	containerFilters.Add("label", projectLabel)

	result, err := s.Client.Daemon().ContainerList(ctx, container.ListOptions{
		All:     all,
		Filters: containerFilters,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list containers for project '%s': %w", project.Name, err)
	}

	return result, nil
}

func (s *ComposeService) StatStack(ctx context.Context, project *types.Project) ([]ContainerStats, error) {
	// Get the list of running containers for the stack.
	stackList, err := s.ListStack(ctx, project, false) // `false` for only running
	if err != nil {
		return nil, err
	}

	result := s.Client.GetStatsFromContainerList(ctx, stackList)
	return result, nil
}

func (s *ComposeService) getProjectImageDigests(ctx context.Context, project *types.Project) (map[string]string, error) {
	digests := make(map[string]string)

	for serviceName, service := range project.Services {
		if service.Image == "" {
			continue
		}

		imageInspect, err := s.Client.Daemon().ImageInspect(ctx, service.Image)
		if err != nil {
			// Image might not exist locally yet
			digests[serviceName] = ""
			continue
		}

		// Use RepoDigests if available, otherwise use the image ID
		if len(imageInspect.RepoDigests) > 0 {
			digests[serviceName] = imageInspect.RepoDigests[0]
		} else {
			digests[serviceName] = imageInspect.ID
		}
	}

	return digests, nil
}

func (s *ComposeService) loadComposeClient(outputStream io.Writer, inputStream io.ReadCloser) (api.Service, error) {
	dockerCli, err := command.NewDockerCli(
		command.WithAPIClient(s.Client.Daemon()),
		command.WithCombinedStreams(outputStream),
		command.WithInputStream(inputStream),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create cli client to docker for compose: %w", err)
	}

	clientOpts := &flags.ClientOptions{}
	if err = dockerCli.Initialize(clientOpts); err != nil {
		return nil, err
	}

	return compose.NewComposeService(dockerCli), nil
}

func (s *ComposeService) loadProject(ctx context.Context, filename string, removeDockman bool) (*types.Project, error) {
	filename = filepath.Join(s.ComposeRoot, filename)
	// will be the parent dir of the compose file else equal to compose root
	workingDir := filepath.Dir(filename)

	options, err := cli.NewProjectOptions(
		[]string{filename},
		// important maintain this order to load .env: workingdir -> env -> os -> load dot env
		cli.WithWorkingDirectory(s.ComposeRoot),
		cli.WithEnvFiles(),
		cli.WithOsEnv,
		cli.WithDotEnv,
		cli.WithDefaultProfiles(),
		cli.WithWorkingDirectory(workingDir),
		cli.WithResolvedPaths(true),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create new project: %w", err)
	}

	project, err := options.LoadProject(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load project: %w", err)
	}

	addServiceLabels(project)
	// Ensure service environment variables
	project, err = project.WithServicesEnvironmentResolved(true)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve services environment: %w", err)
	}

	return project.WithoutUnnecessaryResources(), nil
}

const dockmanImage = "ghcr.io/ra341/dockman"

func (s *ComposeService) withoutDockman(project *types.Project, services ...string) []string {
	// If sftp client exists, it's a remote machine. Do not filter.
	isRemoteDockman := s.Client.Sftp() != nil
	if isRemoteDockman {
		return services
	}

	// Find the name of the service running the "dockman" image.
	var dockmanServiceName string
	for name, config := range project.Services {
		if strings.HasPrefix(config.Image, dockmanImage) {
			dockmanServiceName = name
			log.Info().Msg("Found dockman service to filter from action")
			log.Debug().Str("image", config.Image).Str("service-name", name).
				Msg("This service will be excluded from the final list.")
			break // Found it, no need to keep searching
		}
	}

	// If no service is using the dockman image, there's nothing to filter.
	if dockmanServiceName == "" {
		if len(services) == 0 {
			// empty list implies all services, since no other services were explicitly passed
			return []string{}
		}
		return services
	}

	// Determine which list of services to filter.
	targetServices := services
	// If the user did not provide a specific list of services,
	// use all services from the project as the target.
	if len(services) == 0 {
		targetServices = slices.Collect(maps.Keys(project.Services))
	}

	// Remove the dockman service from the target list and return the result.
	return slices.DeleteFunc(targetServices, func(serviceName string) bool {
		return serviceName == dockmanServiceName
	})
}

// Trim the tag (e.g., ":latest") from the image name for a reliable match
func trimTag(image string) string {
	imageName := image
	if lastColon := strings.LastIndex(image, ":"); lastColon != -1 {
		imageName = image[:lastColon]
	}
	return imageName
}

func (s *ComposeService) sftpProjectFiles(project *types.Project, sfCli *ssh.SftpClient) error {
	for _, service := range project.Services {
		for _, vol := range service.Volumes {
			// We only care about "bind" mounts, which map a host path to a container path.
			// We ignore named volumes, tmpfs, etc.
			if vol.Bind == nil {
				continue
			}

			// `vol.Source` is the local path on the host machine.
			// Because we used `WithResolvedPaths(true)`, this is an absolute path.
			localSourcePath := vol.Source

			// Copy files only whose volume starts with the project's root directory path.
			if !strings.HasPrefix(localSourcePath, s.ComposeRoot) {
				log.Debug().
					Str("local", localSourcePath).
					Msg("Skipping bind mount outside of project root")
				continue
			}

			// Before copying, check if the source file/directory actually exists.
			// It might be a path that gets created by another process or container,
			// so just log/skip if it doesn't exist.
			if !pkg.FileExists(localSourcePath) {
				log.Debug().Str("path", localSourcePath).Msg("bind mount source path not found, skipping...")
				continue
			}

			// The remote destination path will mirror the local absolute path.
			// This ensures the file structure is identical on the remote host.
			remoteDestPath := localSourcePath
			log.Info().
				Str("name", service.Name).
				Str("src (local)", localSourcePath).
				Str("dest (remote)", remoteDestPath).
				Msg("Syncing bind mount for service")

			if err := sfCli.CopyLocalToRemoteSFTP(localSourcePath, remoteDestPath); err != nil {
				return fmt.Errorf("failed to sync bind mount %s for service %s: %w", localSourcePath, service.Name, err)
			}
		}
	}
	return nil
}

func addServiceLabels(project *types.Project) {
	for i, s := range project.Services {
		s.CustomLabels = map[string]string{
			api.ProjectLabel:     project.Name,
			api.ServiceLabel:     s.Name,
			api.VersionLabel:     api.ComposeVersion,
			api.WorkingDirLabel:  "/",
			api.ConfigFilesLabel: strings.Join(project.ComposeFiles, ","),
			api.OneoffLabel:      "False", // default, will be overridden by `run` command
		}

		project.Services[i] = s
	}
}

func (s *ComposeService) syncProjectFilesToHost(project *types.Project) error {
	// nil client implies local client or I done fucked up
	if sfCli := s.Client.Sftp(); sfCli != nil {
		log.Debug().Msg("syncing bind mount to remote host")
		if err := s.sftpProjectFiles(project, sfCli); err != nil {
			return err
		}
	}
	return nil
}
